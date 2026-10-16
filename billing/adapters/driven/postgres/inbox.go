package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// DefaultMaxAttempts is how many times an event is tried before it is dead and pages. With the processor's backoff
// (15 s doubling to 30 min) that is a little over two hours of retries — long enough for a hot tier or a database
// to come back, short enough that a payment nobody can fulfil is not left unnoticed.
const DefaultMaxAttempts = 12

// InboxOptions tune the inbox.
type InboxOptions struct {
	// MaxAttempts is the attempt after which a failed event is dead (default DefaultMaxAttempts).
	MaxAttempts int
}

// Inbox is the durable webhook queue (payment_events, D34): a verified delivery is persisted here and acknowledged in
// one step, and processing happens afterwards, from this table, with leases, retries and a dead state. The UNIQUE
// constraint on (provider_account, provider_event_id) is the replay guard; a clock is injected so every lease and
// backoff is a function of the instant the caller says it is.
type Inbox struct {
	pool  *pgxpool.Pool
	clock mports.Clock
	max   int
}

var _ ports.Inbox = (*Inbox)(nil)

// NewInbox returns the inbox of a database.
func NewInbox(pool *pgxpool.Pool, clock mports.Clock, o InboxOptions) *Inbox {
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = DefaultMaxAttempts
	}
	return &Inbox{pool: pool, clock: clock, max: o.MaxAttempts}
}

// leaseLock serializes lease decisions across every processor: a lease is a short transaction, and what it decides
// — that this customer is free, that this event is due — must not be decided twice at once.
const leaseLock = "billing.payment_events.lease"

// ---- the verified facts of an event, as the database keeps them (not the raw body: that carries PII) ----

type moneyJSON struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}

func moneyOf(m *domain.Money) *moneyJSON {
	if m == nil {
		return nil
	}
	return &moneyJSON{m.MinorUnits, m.Currency}
}

func (m *moneyJSON) money() *domain.Money {
	if m == nil {
		return nil
	}
	return &domain.Money{MinorUnits: m.Minor, Currency: m.Currency}
}

type objectJSON struct {
	CustomerID        string     `json:"customer_id,omitempty"`
	PaymentID         string     `json:"payment_id,omitempty"`
	SubscriptionID    string     `json:"subscription_id,omitempty"`
	PriceID           string     `json:"price_id,omitempty"`
	RefundID          string     `json:"refund_id,omitempty"`
	DisputeID         string     `json:"dispute_id,omitempty"`
	Amount            *moneyJSON `json:"amount,omitempty"`
	AmountRefunded    *moneyJSON `json:"amount_refunded,omitempty"`
	TaxAmount         *moneyJSON `json:"tax_amount,omitempty"`
	Status            string     `json:"status,omitempty"`
	PeriodStart       *time.Time `json:"period_start,omitempty"`
	PeriodEnd         *time.Time `json:"period_end,omitempty"`
	CancelAtPeriodEnd bool       `json:"cancel_at_period_end,omitempty"`
	ReceiptURL        string     `json:"receipt_url,omitempty"`
	FailureReason     string     `json:"failure_reason,omitempty"`
	PaymentMethodRef  string     `json:"payment_method_ref,omitempty"`
}

func encodeObject(o domain.EventObject) ([]byte, error) {
	return json.Marshal(objectJSON{o.ProviderCustomerID, o.ProviderPaymentID, o.ProviderSubscriptionID, o.ProviderPriceID, o.ProviderRefundID,
		o.DisputeID, moneyOf(o.Amount), moneyOf(o.AmountRefunded), moneyOf(o.TaxAmount), o.Status, utcPtr(o.PeriodStart), utcPtr(o.PeriodEnd),
		o.CancelAtPeriodEnd, o.ReceiptURL, o.FailureReason, o.PaymentMethodRef})
}

func decodeObject(raw []byte) (domain.EventObject, error) {
	var j objectJSON
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &j); err != nil {
			return domain.EventObject{}, err
		}
	}
	return domain.EventObject{ProviderCustomerID: j.CustomerID, ProviderPaymentID: j.PaymentID, ProviderSubscriptionID: j.SubscriptionID,
		ProviderPriceID: j.PriceID, ProviderRefundID: j.RefundID, DisputeID: j.DisputeID, Amount: j.Amount.money(), AmountRefunded: j.AmountRefunded.money(),
		TaxAmount: j.TaxAmount.money(), Status: j.Status, PeriodStart: utcPtr(j.PeriodStart), PeriodEnd: utcPtr(j.PeriodEnd),
		CancelAtPeriodEnd: j.CancelAtPeriodEnd, ReceiptURL: j.ReceiptURL, FailureReason: j.FailureReason, PaymentMethodRef: j.PaymentMethodRef}, nil
}

// Persist implements ports.Inbox.
func (in *Inbox) Persist(ctx context.Context, e domain.Event) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	obj, err := encodeObject(e.Object)
	if err != nil {
		return false, fmt.Errorf("postgres: encode event %s: %w", e.ProviderEventID, err)
	}
	now := utc(in.clock.Now())
	tag, err := in.pool.Exec(ctx, `INSERT INTO payment_events (provider_account, provider, provider_event_id, event_type, event_created_at, payload_hash, object,
			status, next_attempt_at, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'received',$8,$8)
		ON CONFLICT (provider_account, provider_event_id) DO NOTHING`,
		e.Account, string(e.Provider), e.ProviderEventID, string(e.Type), utc(e.CreatedAt), e.RawHash, obj, now)
	if err != nil {
		return false, fmt.Errorf("postgres: persist event %s: %w", e.ProviderEventID, mapErr(err))
	}
	return tag.RowsAffected() == 1, nil
}

// Park implements ports.Inbox.
func (in *Inbox) Park(ctx context.Context, provider domain.Provider, account, eventID string, raw []byte, headers http.Header) error {
	hdr, err := json.Marshal(map[string][]string(headers))
	if err != nil {
		return fmt.Errorf("postgres: encode headers: %w", err)
	}
	sum := sha256.Sum256(raw)
	now := utc(in.clock.Now())
	if _, err := in.pool.Exec(ctx, `INSERT INTO payment_events (provider_account, provider, provider_event_id, event_type, payload_hash, parked_body, parked_headers,
			status, next_attempt_at, received_at)
		VALUES ($1,$2,$3,'parked',$4,$5,$6,'unverifiable',$7,$7)
		ON CONFLICT (provider_account, provider_event_id) DO NOTHING`,
		account, string(provider), eventID, hex.EncodeToString(sum[:]), raw, hdr, now); err != nil {
		return fmt.Errorf("postgres: park delivery %s: %w", eventID, mapErr(err))
	}
	return nil
}

// Lease implements ports.Inbox: events due for processing, oldest provider timestamp first, never one customer's two at
// once and never one a live lease holds. It is a single short transaction under a lock shared by every processor, so
// the decisions it makes — this customer is free, this event is due — are made one at a time.
func (in *Inbox) Lease(ctx context.Context, n int, ttl time.Duration) ([]ports.LeasedEvent, error) {
	if n <= 0 {
		return nil, nil
	}
	now := utc(in.clock.Now())
	var out []ports.LeasedEvent
	err := inTx(ctx, in.pool, func(tx pgx.Tx) error {
		if err := lockKey(ctx, tx, leaseLock); err != nil {
			return err
		}
		busy := map[string]bool{}
		brows, err := tx.Query(ctx, `SELECT DISTINCT provider_account || '|' || (object->>'customer_id') FROM payment_events
			WHERE status = 'processing' AND lease_until > $1 AND coalesce(object->>'customer_id', '') <> ''`, now)
		if err != nil {
			return fmt.Errorf("postgres: customers being worked: %w", mapErr(err))
		}
		for brows.Next() {
			var k string
			if err := brows.Scan(&k); err != nil {
				brows.Close()
				return err
			}
			busy[k] = true
		}
		brows.Close()
		if err := brows.Err(); err != nil {
			return err
		}

		rows, err := tx.Query(ctx, `SELECT id::text, provider, provider_account, provider_event_id, event_type, event_created_at, payload_hash, object,
				coalesce(object->>'customer_id', '')
			FROM payment_events
			WHERE (status IN ('received','failed') AND next_attempt_at <= $1) OR (status = 'processing' AND lease_until <= $1)
			ORDER BY event_created_at NULLS LAST, received_at, id LIMIT $2`, now, n*8)
		if err != nil {
			return fmt.Errorf("postgres: due events: %w", mapErr(err))
		}
		type candidate struct {
			id string
			e  domain.Event
		}
		var picked []candidate
		for rows.Next() {
			var id, provider, typ, hash, customer string
			var e domain.Event
			var created *time.Time
			var obj []byte
			if err := rows.Scan(&id, &provider, &e.Account, &e.ProviderEventID, &typ, &created, &hash, &obj, &customer); err != nil {
				rows.Close()
				return fmt.Errorf("postgres: due events: %w", err)
			}
			if len(picked) >= n {
				continue
			}
			key := e.Account + "|" + customer
			if customer != "" {
				if busy[key] {
					continue
				}
				busy[key] = true
			}
			if e.Object, err = decodeObject(obj); err != nil {
				rows.Close()
				return fmt.Errorf("postgres: event %s has an unreadable object: %w", id, err)
			}
			e.Provider, e.Type, e.RawHash, e.CreatedAt = domain.Provider(provider), domain.EventType(typ), hash, fromOpt(created)
			picked = append(picked, candidate{id, e})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, c := range picked {
			var attempts int
			if err := tx.QueryRow(ctx, `UPDATE payment_events SET status = 'processing', lease_until = $2, attempts = attempts + 1 WHERE id = $1::uuid RETURNING attempts`,
				c.id, now.Add(ttl)).Scan(&attempts); err != nil {
				return fmt.Errorf("postgres: lease event %s: %w", c.id, mapErr(err))
			}
			out = append(out, ports.LeasedEvent{ID: c.id, Event: c.e, Attempts: attempts})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (in *Inbox) finish(ctx context.Context, sql string, args ...any) error {
	tag, err := in.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("postgres: inbox: %w", mapErr(err))
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: inbox row %v", errNotFound, args[0])
	}
	return nil
}

// MarkProcessed implements ports.Inbox.
func (in *Inbox) MarkProcessed(ctx context.Context, id, outcome string) error {
	if outcome != "processed" && outcome != "ignored" {
		return fmt.Errorf("%w: outcome %q is neither processed nor ignored", errInvalid, outcome)
	}
	return in.finish(ctx, `UPDATE payment_events SET status = $2, processed_at = $3, lease_until = NULL WHERE id = $1::uuid`, id, outcome, utc(in.clock.Now()))
}

// MarkFailed implements ports.Inbox.
func (in *Inbox) MarkFailed(ctx context.Context, id string, cause error, retryAt time.Time) (bool, error) {
	msg := cause.Error()
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	var dead bool
	err := in.pool.QueryRow(ctx, `UPDATE payment_events SET status = CASE WHEN attempts >= $4 THEN 'dead' ELSE 'failed' END, next_attempt_at = $2, error = $3, lease_until = NULL
		WHERE id = $1::uuid RETURNING status = 'dead'`, id, utc(retryAt), msg, in.max).Scan(&dead)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("%w: inbox row %s", errNotFound, id)
	case err != nil:
		return false, fmt.Errorf("postgres: mark event failed: %w", mapErr(err))
	}
	return dead, nil
}

// LeaseParked implements ports.Inbox.
func (in *Inbox) LeaseParked(ctx context.Context, n int, ttl time.Duration) ([]ports.ParkedDelivery, error) {
	if n <= 0 {
		return nil, nil
	}
	now := utc(in.clock.Now())
	var out []ports.ParkedDelivery
	err := inTx(ctx, in.pool, func(tx pgx.Tx) error {
		if err := lockKey(ctx, tx, leaseLock); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, provider, provider_account, parked_body, parked_headers FROM payment_events
			WHERE status = 'unverifiable' AND next_attempt_at <= $1 AND (lease_until IS NULL OR lease_until <= $1)
			ORDER BY received_at, id LIMIT $2`, now, n)
		if err != nil {
			return fmt.Errorf("postgres: due parked deliveries: %w", mapErr(err))
		}
		for rows.Next() {
			var d ports.ParkedDelivery
			var provider string
			var hdr []byte
			if err := rows.Scan(&d.ID, &provider, &d.Account, &d.Body, &hdr); err != nil {
				rows.Close()
				return err
			}
			d.Provider = domain.Provider(provider)
			var h map[string][]string
			if len(hdr) > 0 {
				if err := json.Unmarshal(hdr, &h); err != nil {
					rows.Close()
					return fmt.Errorf("postgres: parked delivery %s has unreadable headers: %w", d.ID, err)
				}
			}
			d.Header = http.Header(h)
			out = append(out, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			if err := tx.QueryRow(ctx, `UPDATE payment_events SET lease_until = $2, attempts = attempts + 1 WHERE id = $1::uuid RETURNING attempts`,
				out[i].ID, now.Add(ttl)).Scan(&out[i].Attempts); err != nil {
				return fmt.Errorf("postgres: lease parked delivery %s: %w", out[i].ID, mapErr(err))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Promote implements ports.Inbox: a parked delivery that now verifies becomes an ordinary received event and loses
// its raw body. If the provider redelivered the same event normally while it was parked, the event already exists:
// the parked copy is retired and it is reported as not inserted.
func (in *Inbox) Promote(ctx context.Context, id string, e domain.Event) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	obj, err := encodeObject(e.Object)
	if err != nil {
		return false, fmt.Errorf("postgres: encode event %s: %w", e.ProviderEventID, err)
	}
	now := utc(in.clock.Now())
	tag, err := in.pool.Exec(ctx, `UPDATE payment_events SET provider_event_id = $2, event_type = $3, event_created_at = $4, payload_hash = $5, object = $6,
			status = 'received', parked_body = NULL, parked_headers = NULL, attempts = 0, lease_until = NULL, next_attempt_at = $7, error = NULL
		WHERE id = $1::uuid AND status = 'unverifiable' AND provider_account = $8`,
		id, e.ProviderEventID, string(e.Type), utc(e.CreatedAt), e.RawHash, obj, now, e.Account)
	if err != nil {
		if mapped := mapErr(err); errors.Is(mapped, errAlreadyExists) {
			if rerr := in.finish(ctx, `UPDATE payment_events SET status = 'rejected', error = 'duplicate', parked_body = NULL, parked_headers = NULL, lease_until = NULL WHERE id = $1::uuid`, id); rerr != nil {
				return false, rerr
			}
			return false, nil
		}
		return false, fmt.Errorf("postgres: promote parked delivery %s: %w", id, mapErr(err))
	}
	if tag.RowsAffected() == 0 {
		return false, fmt.Errorf("%w: no parked delivery %s for account %s", errNotFound, id, e.Account)
	}
	return true, nil
}

// RetryParked implements ports.Inbox.
func (in *Inbox) RetryParked(ctx context.Context, id string, retryAt time.Time) error {
	return in.finish(ctx, `UPDATE payment_events SET next_attempt_at = $2, lease_until = NULL WHERE id = $1::uuid AND status = 'unverifiable'`, id, utc(retryAt))
}

// Reject implements ports.Inbox.
func (in *Inbox) Reject(ctx context.Context, id, reason string) error {
	return in.finish(ctx, `UPDATE payment_events SET status = 'rejected', error = $2, parked_body = NULL, parked_headers = NULL, lease_until = NULL WHERE id = $1::uuid`, id, reason)
}

// Oldest implements ports.Inbox: the oldest receipt among events not yet fulfilled — parked ones included, dead ones not
// (they page on their own, and would pin the age forever).
func (in *Inbox) Oldest(ctx context.Context) (time.Time, bool, error) {
	var at *time.Time
	if err := in.pool.QueryRow(ctx, `SELECT min(received_at) FROM payment_events WHERE status IN ('received','processing','failed','unverifiable')`).Scan(&at); err != nil {
		return time.Time{}, false, fmt.Errorf("postgres: oldest unfulfilled event: %w", mapErr(err))
	}
	if at == nil {
		return time.Time{}, false, nil
	}
	return at.UTC(), true, nil
}

// Purge implements ports.Inbox: finished rows past retention. It never touches an event that is waiting or dead.
func (in *Inbox) Purge(ctx context.Context, olderThan time.Time) (int, error) {
	tag, err := in.pool.Exec(ctx, `DELETE FROM payment_events WHERE (status IN ('processed','ignored') AND processed_at < $1) OR (status = 'rejected' AND received_at < $1)`, utc(olderThan))
	if err != nil {
		return 0, fmt.Errorf("postgres: purge the inbox: %w", mapErr(err))
	}
	return int(tag.RowsAffected()), nil
}
