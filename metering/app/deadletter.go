package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// DeadLetterGroup is the sweeper's consumer group. It is part of the deployment: a group that changed between releases would hand
// every dead letter to the old group and to the new one.
const DeadLetterGroup = "dead-letter-sweeper"

// DefaultReplayLease is how long a replay's claim on a letter holds before another replay may take it over.
const DefaultReplayLease = time.Minute

// DeadLetterDeps are what the sweeper needs.
type DeadLetterDeps struct {
	Store   ports.DeadLetterStore
	Bus     ports.Bus
	Metrics ports.Metrics // default: no-op
	// Prefix is the deployment's subject prefix (`billing`): the sweeper reads `<Prefix>.dlq.>`.
	Prefix string
	// Lease is how long a replay's claim holds; default DefaultReplayLease. It need only exceed one publish.
	Lease time.Duration
}

// DeadLetters is the other half of the Bus's promise. A message that fails MaxAttempts times is published whole to
// `<prefix>.dlq.<subject>` and acknowledged; this parks each such message in the store and pages once. It never delivers a parked
// message again: the only way back is Replay, which an operator asks for by name.
//
// Redelivery below the cap is the Bus's, and it is why nothing here retries.
type DeadLetters struct {
	d   DeadLetterDeps
	met ports.Metrics
}

// NewDeadLetters validates the dependencies.
func NewDeadLetters(d DeadLetterDeps) (*DeadLetters, error) {
	var errs []error
	if d.Store == nil || d.Bus == nil {
		errs = append(errs, errors.New("a DeadLetterStore and a Bus are required"))
	}
	if strings.TrimSpace(d.Prefix) == "" {
		errs = append(errs, errors.New("a subject prefix is required"))
	}
	if d.Lease < 0 {
		errs = append(errs, errors.New("the replay lease cannot be negative"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering dead letters: %w", err)
	}
	if d.Lease == 0 {
		d.Lease = DefaultReplayLease
	}
	met := d.Metrics
	if met == nil {
		met = nopMetrics{}
	}
	return &DeadLetters{d: d, met: met}, nil
}

// Subscribe joins the dead-letter subjects. Not being able to is the caller's failure to report: a worker that does not sweep is
// one whose dead letters pile up in a stream nobody reads.
func (s *DeadLetters) Subscribe(ctx context.Context) (ports.Subscription, error) {
	return s.d.Bus.Subscribe(ctx, s.d.Prefix+".dlq.>", DeadLetterGroup, s.Handle)
}

// The envelope both Bus adapters publish (bus-subjects.md): the original message and what became of it.
type deadLetterEnvelope struct {
	Subject  string `json:"subject"`
	ID       string `json:"id"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	Payload  []byte `json:"payload"`
}

// Handle parks one dead letter and pages if this call is the one that parked it. A failure to park is returned, so the Bus
// delivers the envelope again: the letter is in the stream until it is in the store.
func (s *DeadLetters) Handle(ctx context.Context, m ports.Message) error {
	l := s.letter(m)
	stored, created, err := s.d.Store.Park(ctx, l)
	if err != nil {
		return fmt.Errorf("park dead letter of %s: %w", l.Subject, err)
	}
	if created {
		s.met.Count(ports.MetricDeadLettered, 1, ports.Labels{"realm": stored.Realm})
	}
	return nil
}

// letter reads the envelope. One that is not an envelope is parked as it arrived, under the subject it arrived on: a message
// nobody can read is the one an operator most needs to see, and the bus delivered it here, not to be dropped.
func (s *DeadLetters) letter(m ports.Message) ports.DeadLetter {
	var env deadLetterEnvelope
	if err := json.Unmarshal(m.Payload, &env); err != nil || env.Subject == "" || env.ID == "" {
		return ports.DeadLetter{
			Realm: ports.GlobalRealm, Subject: m.Subject, SourceID: m.ID, Payload: append([]byte(nil), m.Payload...),
			Attempts: m.Attempts, LastError: "not a dead-letter envelope: parked as it arrived",
		}
	}
	return ports.DeadLetter{
		Realm: realmOf(env.Payload), Subject: env.Subject, SourceID: env.ID, Payload: env.Payload,
		Attempts: env.Attempts, LastError: env.Error,
	}
}

// realmOf is the realm a payload names, or the global realm when it names none or names one that is not a realm.
func realmOf(payload []byte) string {
	var p struct {
		Realm string `json:"realm"`
	}
	if json.Unmarshal(payload, &p) != nil || p.Realm == "" || domain.Realm(p.Realm).Validate() != nil {
		return ports.GlobalRealm
	}
	return p.Realm
}

// ReplayOutcome says what a Replay did.
type ReplayOutcome string

const (
	// Replayed: this call re-published the letter.
	Replayed ReplayOutcome = "replayed"
	// AlreadyReplayed: an earlier replay did. Nothing was published.
	AlreadyReplayed ReplayOutcome = "already_replayed"
	// ReplayInProgress: another replay holds the letter and has not finished. Nothing was published; asking again after its lease
	// finishes the work if it was killed.
	ReplayInProgress ReplayOutcome = "in_progress"
)

// Replay re-publishes a parked message to the subject it failed on. It is idempotent under retry and under concurrency: one
// caller claims the letter, publishes, and records that it did; every other caller is told what became of it. A publish that
// fails gives the claim back, so the letter is still parked and the next call tries again. A caller killed between the publish
// and the record leaves the claim to lapse, and the next replay publishes it again: a duplicate delivery, which every handler
// of a bus subject is built to absorb.
func (s *DeadLetters) Replay(ctx context.Context, id string) (ReplayOutcome, ports.DeadLetter, error) {
	l, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return "", ports.DeadLetter{}, err
	}
	if strings.HasPrefix(l.Subject, s.d.Prefix+".dlq.") {
		return "", l, fmt.Errorf("%w: dead letter %s is not a bus envelope, so there is no subject to replay it to; its payload is kept for an operator to read", domain.ErrInvalid, id)
	}
	l, claimed, err := s.d.Store.ClaimReplay(ctx, id, s.d.Lease)
	if err != nil {
		return "", l, err
	}
	if !claimed {
		if l.ReplayedAt != nil {
			return AlreadyReplayed, l, nil
		}
		return ReplayInProgress, l, nil
	}
	if err := s.d.Bus.Publish(ctx, l.Subject, l.Payload); err != nil {
		if rerr := s.d.Store.ReleaseReplay(context.WithoutCancel(ctx), id); rerr != nil {
			err = errors.Join(err, fmt.Errorf("release the claim: %w", rerr))
		}
		return "", l, fmt.Errorf("replay dead letter %s to %s: %w", id, l.Subject, err)
	}
	if err := s.d.Store.FinishReplay(ctx, id); err != nil {
		return "", l, fmt.Errorf("record the replay of dead letter %s: %w", id, err)
	}
	l, err = s.d.Store.Get(ctx, id)
	if err != nil {
		return "", l, err
	}
	return Replayed, l, nil
}

// ExportGauges publishes the letters nobody has replayed, per realm: the number the page is written against, and one that comes
// from the store and not from a counter in a process that may since have died.
func (s *DeadLetters) ExportGauges(ctx context.Context) error {
	open, err := s.d.Store.Open(ctx)
	if err != nil {
		return err
	}
	for realm, n := range open {
		s.met.Gauge(ports.MetricDeadLetters, n, ports.Labels{"realm": realm})
	}
	return nil
}
