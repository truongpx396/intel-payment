package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// journalLease is how long a claimed entry stays out of Due before another tick may take it. A
// replay through the hot function is idempotent, so a lease that expires while a slow replay is
// still running costs nothing but a REPLAY.
const journalLease = 30 * time.Second

// Journal is the spend journal: the durable home of a usage intent that has not reached the hot tier
// yet — on every call under Settlement=journal, and during a hot-store outage in any mode, so
// `fail_open` means served before metered, never served unmetered (invariant 16).
type Journal struct{ pool *pgxpool.Pool }

var _ ports.JournalStore = (*Journal)(nil)

// NewJournal builds the journal over a pool.
func NewJournal(pool *pgxpool.Pool) *Journal { return &Journal{pool: pool} }

// chargeDoc is the JSONB form of a Charge. It is its own type so the wire form of the journal does
// not silently change when the domain type gains a field.
type chargeDoc struct {
	Amount          int64                    `json:"amount"`
	Reason          string                   `json:"reason,omitempty"`
	Resource        string                   `json:"resource,omitempty"`
	Subjects        map[string]string        `json:"subjects,omitempty"`
	Quantities      []domain.PayloadQuantity `json:"q,omitempty"`
	Ref             map[string]string        `json:"ref,omitempty"`
	RateKey         string                   `json:"rate_key,omitempty"`
	RateCardVersion string                   `json:"rate_card"`
	CostMicros      int64                    `json:"cost_micros,omitempty"`
	OccurredAt      time.Time                `json:"occurred_at"`
}

func toDoc(c domain.Charge) chargeDoc {
	d := chargeDoc{Amount: int64(c.Amount), Reason: c.Reason, Resource: c.Resource, Ref: c.Ref, RateKey: c.RateKey,
		RateCardVersion: c.RateCardVersion, CostMicros: c.CostMicros, OccurredAt: c.OccurredAt.UTC()}
	if len(c.Subjects) > 0 {
		d.Subjects = c.Subjects
	}
	for _, q := range c.Quantities {
		d.Quantities = append(d.Quantities, domain.PayloadQuantity{Unit: string(q.Unit), Amount: q.Amount})
	}
	return d
}

func (d chargeDoc) charge(s domain.Scope, key string) domain.Charge {
	c := domain.Charge{Scope: s, Amount: domain.Credits(d.Amount), IdemKey: key, Reason: d.Reason, Resource: d.Resource,
		Subjects: d.Subjects, Ref: d.Ref, RateKey: d.RateKey, RateCardVersion: d.RateCardVersion,
		CostMicros: d.CostMicros, OccurredAt: d.OccurredAt}
	for _, q := range d.Quantities {
		c.Quantities = append(c.Quantities, domain.Quantity{Unit: domain.Unit(q.Unit), Amount: q.Amount})
	}
	return c
}

// Put records a charge. The same key with the same fingerprint is an idempotent retry (created is
// false); the same key with a different fingerprint is ErrIdemConflict.
func (j *Journal) Put(ctx context.Context, e ports.JournalEntry) (bool, error) {
	c := e.Charge.Scope.Normalized()
	doc, err := json.Marshal(toDoc(e.Charge))
	if err != nil {
		return false, fmt.Errorf("postgres: journal payload: %w", err)
	}
	tag, err := j.pool.Exec(ctx, `
		INSERT INTO spend_journal (realm, scope_kind, scope_id, idem_key, fingerprint, amount, rate_card_version, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (realm, scope_kind, scope_id, idem_key) DO NOTHING`,
		string(c.Realm), c.Kind, c.ID, e.Charge.IdemKey, e.Fingerprint, int64(e.Charge.Amount), e.Charge.RateCardVersion, doc)
	if err != nil {
		return false, fmt.Errorf("postgres: journal put: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	var fp string
	if err := j.pool.QueryRow(ctx, `
		SELECT fingerprint FROM spend_journal WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND idem_key = $4`,
		string(c.Realm), c.Kind, c.ID, e.Charge.IdemKey).Scan(&fp); err != nil {
		return false, fmt.Errorf("postgres: journal read back: %w", err)
	}
	if fp != e.Fingerprint {
		return false, fmt.Errorf("%w: journal key %q", domain.ErrIdemConflict, e.Charge.IdemKey)
	}
	return false, nil
}

// Due claims up to limit pending entries whose next attempt is at or before now, oldest first, by
// pushing their next attempt out by the lease. Two concurrent ticks therefore never take the same
// row (and if they did, the replay is idempotent).
func (j *Journal) Due(ctx context.Context, now time.Time, limit int) ([]ports.JournalEntry, error) {
	rows, err := j.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM spend_journal
			WHERE status = 'pending' AND next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED)
		UPDATE spend_journal j SET next_attempt_at = $1 + $3::interval
		FROM due WHERE j.id = due.id
		RETURNING j.id::text, j.realm, j.scope_kind, j.scope_id, j.idem_key, j.fingerprint, j.attempts,
		          j.next_attempt_at, j.created_at, j.payload`, now, limit, fmt.Sprintf("%d seconds", int(journalLease.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("postgres: journal due: %w", err)
	}
	defer rows.Close()
	var out []ports.JournalEntry
	for rows.Next() {
		var (
			e                    ports.JournalEntry
			realm, kind, id, key string
			doc                  []byte
		)
		if err := rows.Scan(&e.ID, &realm, &kind, &id, &key, &e.Fingerprint, &e.Attempts, &e.NextAttemptAt, &e.CreatedAt, &doc); err != nil {
			return nil, fmt.Errorf("postgres: scan journal: %w", err)
		}
		var d chargeDoc
		if err := json.Unmarshal(doc, &d); err != nil {
			return nil, fmt.Errorf("postgres: journal entry %s is unreadable: %w", e.ID, err)
		}
		e.Charge = d.charge(domain.Scope{Realm: domain.Realm(realm), Kind: kind, ID: id}, key)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Retry records a failed replay and schedules the next attempt.
func (j *Journal) Retry(ctx context.Context, id string, attempts int, next time.Time, _ string) error {
	tag, err := j.pool.Exec(ctx, `UPDATE spend_journal SET attempts = $2, next_attempt_at = $3 WHERE id = $1::uuid AND status = 'pending'`, id, attempts, next)
	if err != nil {
		return fmt.Errorf("postgres: journal retry: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Pending counts unsettled entries for a realm (metering_journal_pending).
func (j *Journal) Pending(ctx context.Context, realm domain.Realm) (int64, error) {
	var n int64
	if err := j.pool.QueryRow(ctx, `SELECT count(*) FROM spend_journal WHERE realm = $1 AND status = 'pending'`, string(realm.Or())).Scan(&n); err != nil {
		return 0, fmt.Errorf("postgres: journal pending: %w", err)
	}
	return n, nil
}
