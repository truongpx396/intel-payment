package postgres

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// Payments are the fiat transactions. A payment is NOT a ledger row; its idempotency key joins the two. The
// database's CHECK constraints are what stops a refund past the payment or a clawback past the grant, and Update's
// advisory lock is what makes "revoke exactly" hold when two refund reports meet.
type Payments struct{ pool *pgxpool.Pool }

var _ ports.Payments = (*Payments)(nil)

// NewPayments returns the payments of a database.
func NewPayments(pool *pgxpool.Pool) *Payments { return &Payments{pool: pool} }

const paymentColumns = `p.realm, p.scope_kind, p.scope_id, p.provider_account, p.provider_payment_id, coalesce(pl.code, ''), p.kind,
	p.amount_minor, p.currency, p.tax_minor, p.credits_granted, p.pool, p.refunded_minor, p.credits_revoked, p.status,
	coalesce(p.dispute_id, ''), coalesce(p.dispute_status, ''), p.dispute_clawed, coalesce(p.receipt_url, ''),
	coalesce(p.failure_reason, ''), p.idem_key, p.created_at`

const paymentFrom = ` FROM payments p LEFT JOIN plans pl ON pl.id = p.plan_id`

// scanPayment reads the payment columns, and any extra the query appends (the id a cursor needs).
func scanPayment(row pgx.Row, extra ...any) (domain.Payment, error) {
	var p domain.Payment
	var realm, pool, status string
	var tax *int64
	var amount int64
	var currency string
	var created time.Time
	dest := []any{&realm, &p.Scope.Kind, &p.Scope.ID, &p.Account, &p.ProviderPaymentID, &p.PlanCode, &p.Kind, &amount, &currency, &tax,
		&p.CreditsGranted, &pool, &p.RefundedMinor, &p.CreditsRevoked, &status, &p.DisputeID, &p.DisputeStatus, &p.DisputeClawed,
		&p.ReceiptURL, &p.FailureReason, &p.IdemKey, &created}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return domain.Payment{}, err
	}
	p.Scope.Realm, p.Pool, p.Status = domain.Realm(realm), domain.Pool(pool), domain.PaymentStatus(status)
	p.Amount = domain.Money{MinorUnits: amount, Currency: strings.TrimSpace(currency)}
	if tax != nil {
		p.TaxAmount = &domain.Money{MinorUnits: *tax, Currency: p.Amount.Currency}
	}
	p.CreatedAt = created.UTC()
	return p, nil
}

// Get implements ports.Payments.
func (s *Payments) Get(ctx context.Context, account, providerPaymentID string) (domain.Payment, bool, error) {
	return getPayment(ctx, s.pool, account, providerPaymentID)
}

func getPayment(ctx context.Context, q db, account, id string) (domain.Payment, bool, error) {
	p, err := scanPayment(q.QueryRow(ctx, `SELECT `+paymentColumns+paymentFrom+` WHERE p.provider_account = $1 AND p.provider_payment_id = $2`, account, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Payment{}, false, nil
	case err != nil:
		return domain.Payment{}, false, fmt.Errorf("postgres: payment %s: %w", id, mapErr(err))
	}
	return p, true, nil
}

// Upsert implements ports.Payments.
func (s *Payments) Upsert(ctx context.Context, p domain.Payment) error {
	return upsertPayment(ctx, s.pool, p)
}

func upsertPayment(ctx context.Context, q db, p domain.Payment) error {
	if err := p.Scope.Validate(); err != nil {
		return err
	}
	var planID *string
	if p.PlanCode != "" {
		var id string
		err := q.QueryRow(ctx, `SELECT id::text FROM plans WHERE realm = $1 AND code = $2`, realmOf(p.Scope), p.PlanCode).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: payment %s is for plan %s, which realm %s does not sell", domain.ErrUnknownPrice, p.ProviderPaymentID, p.PlanCode, p.Scope.Realm.Or())
		} else if err != nil {
			return fmt.Errorf("postgres: plan %s: %w", p.PlanCode, mapErr(err))
		}
		planID = &id
	}
	var tax *int64
	if p.TaxAmount != nil {
		if p.TaxAmount.Currency != p.Amount.Currency {
			return fmt.Errorf("%w: payment %s reports tax in %s on an amount in %s", errInvalid, p.ProviderPaymentID, p.TaxAmount.Currency, p.Amount.Currency)
		}
		tax = &p.TaxAmount.MinorUnits
	}
	var created *time.Time
	if !p.CreatedAt.IsZero() {
		c := utc(p.CreatedAt)
		created = &c
	}
	_, err := q.Exec(ctx, `INSERT INTO payments (realm, scope_kind, scope_id, provider_account, provider_payment_id, plan_id, kind, amount_minor, currency,
			tax_minor, credits_granted, pool, refunded_minor, credits_revoked, status, dispute_id, dispute_status, dispute_clawed, receipt_url,
			failure_reason, idem_key, created_at)
		VALUES ($1,$2,$3,$4,$5,$6::uuid,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21, coalesce($22, now()))
		ON CONFLICT (provider_account, provider_payment_id) DO UPDATE SET plan_id = EXCLUDED.plan_id, kind = EXCLUDED.kind,
			amount_minor = EXCLUDED.amount_minor, currency = EXCLUDED.currency, tax_minor = EXCLUDED.tax_minor,
			credits_granted = EXCLUDED.credits_granted, pool = EXCLUDED.pool, refunded_minor = EXCLUDED.refunded_minor,
			credits_revoked = EXCLUDED.credits_revoked, status = EXCLUDED.status, dispute_id = EXCLUDED.dispute_id,
			dispute_status = EXCLUDED.dispute_status, dispute_clawed = EXCLUDED.dispute_clawed, receipt_url = EXCLUDED.receipt_url,
			failure_reason = EXCLUDED.failure_reason, idem_key = EXCLUDED.idem_key, updated_at = now()`,
		realmOf(p.Scope), p.Scope.Kind, p.Scope.ID, p.Account, p.ProviderPaymentID, planID, p.Kind, p.Amount.MinorUnits, p.Amount.Currency,
		tax, int64(p.CreditsGranted), string(p.Pool.Or()), p.RefundedMinor, int64(p.CreditsRevoked), string(p.Status), nilIfEmpty(p.DisputeID),
		nilIfEmpty(p.DisputeStatus), int64(p.DisputeClawed), nilIfEmpty(p.ReceiptURL), nilIfEmpty(p.FailureReason), p.IdemKey, created)
	if err != nil {
		return fmt.Errorf("postgres: store payment %s: %w", p.ProviderPaymentID, mapErr(err))
	}
	return nil
}

// Update implements ports.Payments: fn runs on the payment under an exclusive lock on its name, in one transaction with
// the write, so two refund reports for one payment — or the first report and the event that creates the payment —
// cannot read the same figures. fn may call Meter.Grant: every call it makes is idempotent, so a failed commit is a
// retry, not a second effect.
func (s *Payments) Update(ctx context.Context, account, providerPaymentID string, fn func(domain.Payment, bool) (domain.Payment, error)) (domain.Payment, error) {
	var out domain.Payment
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockKey(ctx, tx, "payment:"+account+"|"+providerPaymentID); err != nil {
			return err
		}
		cur, found, err := getPayment(ctx, tx, account, providerPaymentID)
		if err != nil {
			return err
		}
		next, err := fn(cur, found)
		if err != nil {
			return err
		}
		if err := upsertPayment(ctx, tx, next); err != nil {
			return err
		}
		out = next
		return nil
	})
	if err != nil {
		return domain.Payment{}, err
	}
	stored, _, err := getPayment(ctx, s.pool, account, providerPaymentID)
	if err != nil {
		return out, err
	}
	return stored, nil
}

// List implements ports.Payments: newest first, a keyset page at a time (created_at, id), so every payment appears
// exactly once across pages however many are written between them.
func (s *Payments) List(ctx context.Context, scope domain.Scope, cursor string, limit int) ([]domain.Payment, string, error) {
	n, err := pageSize(limit)
	if err != nil {
		return nil, "", err
	}
	sql := `SELECT ` + paymentColumns + `, p.id::text` + paymentFrom + ` WHERE p.realm = $1 AND p.scope_kind = $2 AND p.scope_id = $3`
	args := []any{realmOf(scope), scope.Kind, scope.ID}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		sql += ` AND (p.created_at, p.id) < ($4, $5::uuid)`
		args = append(args, at, id)
	}
	sql += fmt.Sprintf(` ORDER BY p.created_at DESC, p.id DESC LIMIT %d`, n+1)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list payments of %s: %w", scope.Tag(), mapErr(err))
	}
	defer rows.Close()
	var out []domain.Payment
	var lastAt time.Time
	var lastID, nextID string
	for rows.Next() {
		var id string
		p, err := scanPayment(rows, &id)
		if err != nil {
			return nil, "", fmt.Errorf("postgres: list payments of %s: %w", scope.Tag(), err)
		}
		if len(out) == n { // one past the page: there is more
			nextID = id
			break
		}
		out = append(out, p)
		lastAt, lastID = p.CreatedAt, id
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if nextID != "" {
		next = encodeCursor(lastAt, lastID)
	}
	return out, next, nil
}

// cursors are opaque: the microsecond of the last row and its id.
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: bad cursor", errInvalid)
	}
	micros, id, ok := strings.Cut(string(raw), "|")
	m, perr := strconv.ParseInt(micros, 10, 64)
	if !ok || perr != nil || len(id) != 36 {
		return time.Time{}, "", fmt.Errorf("%w: bad cursor", errInvalid)
	}
	return time.UnixMicro(m).UTC(), id, nil
}
