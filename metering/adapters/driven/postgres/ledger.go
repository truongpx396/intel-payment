package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

var _ ports.LedgerReader = (*Books)(nil)

// LedgerRows reads one scope's booked rows, newest first, as a keyset over (created_at, id): a page after a
// cursor is exactly the rows older than it, so every row appears once however many are booked between two
// reads, and rows that share a timestamp cannot be skipped or repeated. created_at is clock_timestamp(),
// assigned under the scope's lock, so for one scope it is commit order.
func (b *Books) LedgerRows(ctx context.Context, q ports.LedgerQuery) (ports.LedgerPage, error) {
	if err := q.Scope.Validate(); err != nil {
		return ports.LedgerPage{}, err
	}
	if (q.SubjectKind == "") != (q.SubjectID == "") {
		return ports.LedgerPage{}, fmt.Errorf("%w: a member-scoped read needs both subject_kind and subject_id", domain.ErrInvalid)
	}
	limit := q.Limit
	switch {
	case limit == 0:
		limit = ports.DefaultLedgerPage
	case limit < 0 || limit > ports.MaxLedgerPage:
		return ports.LedgerPage{}, fmt.Errorf("%w: limit must be 1..%d", domain.ErrInvalid, ports.MaxLedgerPage)
	}
	s := q.Scope.Normalized()

	args := []any{string(s.Realm), s.Kind, s.ID}
	where := `realm = $1 AND scope_kind = $2 AND scope_id = $3`
	if q.SubjectKind != "" {
		member, _ := json.Marshal(map[string]string{q.SubjectKind: q.SubjectID})
		args = append(args, string(member))
		where += fmt.Sprintf(` AND subjects @> $%d::jsonb`, len(args))
	}
	if q.After != "" {
		at, id, err := decodeCursor(q.After)
		if err != nil {
			return ports.LedgerPage{}, err
		}
		args = append(args, at, id)
		where += fmt.Sprintf(` AND (created_at, id) < ($%d, $%d::uuid)`, len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := b.pool.Query(ctx, `
		SELECT id::text, created_at, pool, delta, operation_type, counter_scope_kind, counter_scope_id, idem_key, seq_from, seq_to, event_count,
		       rate_card_version, cost_micros, resource, rate_key, quantities, subjects, actor_id, ref, occurred_at
		  FROM credit_ledger WHERE `+where+fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return ports.LedgerPage{}, fmt.Errorf("postgres: read ledger: %w", err)
	}
	defer rows.Close()

	var page ports.LedgerPage
	for rows.Next() {
		var (
			e                    ports.LedgerEntry
			pool                 string
			cpKind, cpID         *string
			rcv, resource, rkey  *string
			actor                *string
			cost                 *int64
			quantities, subjects []byte
			ref                  []byte
			occurred             *time.Time
		)
		if err := rows.Scan(&e.ID, &e.CreatedAt, &pool, &e.Delta, &e.OperationType, &cpKind, &cpID, &e.IdemKey, &e.SeqFrom, &e.SeqTo, &e.EventCount,
			&rcv, &cost, &resource, &rkey, &quantities, &subjects, &actor, &ref, &occurred); err != nil {
			return ports.LedgerPage{}, fmt.Errorf("postgres: scan ledger row: %w", err)
		}
		e.Scope, e.Pool = s, domain.Pool(pool)
		if cpKind != nil && cpID != nil {
			e.Counterparty = &domain.Scope{Realm: s.Realm, Kind: *cpKind, ID: *cpID}
		}
		e.RateCardVersion, e.Resource, e.RateKey = deref(rcv), deref(resource), deref(rkey)
		if cost != nil {
			e.CostMicros = *cost
		}
		e.ActorID = deref(actor)
		if occurred != nil {
			e.OccurredAt = *occurred
		}
		if len(quantities) > 0 {
			var qs []domain.PayloadQuantity // the shape Book writes
			if err := json.Unmarshal(quantities, &qs); err != nil {
				return ports.LedgerPage{}, fmt.Errorf("postgres: ledger row %s: quantities: %w", e.ID, err)
			}
			for _, q := range qs {
				e.Quantities = append(e.Quantities, domain.Quantity{Unit: domain.Unit(q.Unit), Amount: q.Amount})
			}
		}
		if len(subjects) > 0 {
			if err := json.Unmarshal(subjects, &e.Subjects); err != nil {
				return ports.LedgerPage{}, fmt.Errorf("postgres: ledger row %s: subjects: %w", e.ID, err)
			}
		}
		if len(ref) > 0 {
			if err := json.Unmarshal(ref, &e.Ref); err != nil {
				return ports.LedgerPage{}, fmt.Errorf("postgres: ledger row %s: ref: %w", e.ID, err)
			}
		}
		page.Rows = append(page.Rows, e)
	}
	if err := rows.Err(); err != nil {
		return ports.LedgerPage{}, fmt.Errorf("postgres: read ledger: %w", err)
	}
	if len(page.Rows) > limit {
		page.Rows = page.Rows[:limit]
		last := page.Rows[limit-1]
		page.Next = encodeCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// The cursor is the last row's (created_at, id), opaque to a caller. It is not signed: it grants nothing a
// scope-bound query would not, and a forged one only chooses where to start reading the caller's own rows.
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(at.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: the cursor is not one this service issued", domain.ErrInvalid)
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: the cursor is not one this service issued", domain.ErrInvalid)
	}
	at, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: the cursor is not one this service issued", domain.ErrInvalid)
	}
	if !uuidPattern.MatchString(id) {
		return time.Time{}, "", fmt.Errorf("%w: the cursor is not one this service issued", domain.ErrInvalid)
	}
	return at, id, nil
}

// Granted is everything the books ever credited to the scope: every positive row, less the writeoffs —
// forgiven debt is not a grant. A transfer in is a grant to its destination. The denominator of "12,480 left of
// 15,000". A sum past int64 is an error, never a wrapped number.
func (b *Books) Granted(ctx context.Context, s domain.Scope) (domain.Credits, error) {
	if err := s.Validate(); err != nil {
		return 0, err
	}
	s = s.Normalized()
	var total int64
	err := b.pool.QueryRow(ctx, `SELECT COALESCE(sum(delta), 0)::bigint FROM credit_ledger
		WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND delta > 0 AND operation_type <> 'writeoff'`,
		string(s.Realm), s.Kind, s.ID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("postgres: sum granted: %w", err)
	}
	return domain.Credits(total), nil
}
