package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
)

// Subs are subscriptions. Their status has exactly one writer, the processor, and the order in which states apply is
// the domain's own total order (Sub.Apply): the store holds an exclusive lock on the subscription's name, reads the
// state held, asks the domain whether the new one supersedes it, and writes — so the SQL does not reimplement the
// order, and concurrent appliers converge on the newest state whatever their interleaving.
type Subs struct{ pool *pgxpool.Pool }

var _ ports.Subs = (*Subs)(nil)

// NewSubs returns the subscriptions of a database.
func NewSubs(pool *pgxpool.Pool) *Subs { return &Subs{pool: pool} }

const subColumns = `s.realm, s.scope_kind, s.scope_id, pl.code, s.provider_account, s.provider_subscription_id, s.status, s.current_period_start,
	s.current_period_end, s.cancel_at_period_end, s.grace_until, s.grace_lapsed_at, s.last_event_at`

const subFrom = ` FROM subscriptions s JOIN plans pl ON pl.id = s.plan_id`

func scanSub(row pgx.Row) (domain.Sub, error) {
	var s domain.Sub
	var realm, status string
	var start, end, graceUntil, lapsed, last *time.Time
	if err := row.Scan(&realm, &s.Scope.Kind, &s.Scope.ID, &s.PlanCode, &s.Account, &s.ProviderSubscriptionID, &status, &start, &end,
		&s.CancelAtPeriodEnd, &graceUntil, &lapsed, &last); err != nil {
		return domain.Sub{}, err
	}
	s.Scope.Realm, s.Status = domain.Realm(realm), domain.SubStatus(status)
	s.CurrentPeriodStart, s.CurrentPeriodEnd, s.LastEventAt = fromOpt(start), fromOpt(end), fromOpt(last)
	s.GraceUntil, s.GraceLapsedAt = utcPtr(graceUntil), utcPtr(lapsed)
	return s, nil
}

// Get implements ports.Subs: the scope's live subscription, else the newest it ever had.
func (s *Subs) Get(ctx context.Context, scope domain.Scope) (domain.Sub, bool, error) {
	sub, err := scanSub(s.pool.QueryRow(ctx, `SELECT `+subColumns+subFrom+` WHERE s.realm = $1 AND s.scope_kind = $2 AND s.scope_id = $3
		ORDER BY (s.status IN ('trialing','active','past_due','paused')) DESC, s.last_event_at DESC NULLS LAST LIMIT 1`, realmOf(scope), scope.Kind, scope.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.Sub{}, false, nil
	case err != nil:
		return domain.Sub{}, false, fmt.Errorf("postgres: subscription of %s: %w", scope.Tag(), mapErr(err))
	}
	return sub, true, nil
}

// ApplyIfNewer implements ports.Subs.
func (s *Subs) ApplyIfNewer(ctx context.Context, sub domain.Sub, at time.Time) (bool, error) {
	if err := sub.Scope.Validate(); err != nil {
		return false, err
	}
	// The database keeps microseconds; compare what it will hold.
	sub.CurrentPeriodStart, sub.CurrentPeriodEnd = utc(sub.CurrentPeriodStart), utc(sub.CurrentPeriodEnd)
	sub.GraceUntil, sub.GraceLapsedAt = utcPtr(sub.GraceUntil), utcPtr(sub.GraceLapsedAt)
	at = utc(at)
	var applied bool
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockKey(ctx, tx, "subscription:"+sub.Account+"|"+sub.ProviderSubscriptionID); err != nil {
			return err
		}
		cur, err := scanSub(tx.QueryRow(ctx, `SELECT `+subColumns+subFrom+` WHERE s.provider_account = $1 AND s.provider_subscription_id = $2`, sub.Account, sub.ProviderSubscriptionID))
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			cur = domain.Sub{}
		case err != nil:
			return fmt.Errorf("postgres: subscription %s: %w", sub.ProviderSubscriptionID, mapErr(err))
		default:
			if cur.Scope.Normalized() != sub.Scope.Normalized() {
				return fmt.Errorf("%w: subscription %s belongs to %s and is not %s's to write", errInvalid, sub.ProviderSubscriptionID, cur.Scope.Tag(), sub.Scope.Tag())
			}
		}
		next, ok := cur.Apply(sub, at)
		if !ok {
			return nil
		}
		var planID string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM plans WHERE realm = $1 AND code = $2`, realmOf(next.Scope), next.PlanCode).Scan(&planID); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: subscription %s is on plan %s, which realm %s does not sell", domain.ErrUnknownPrice, next.ProviderSubscriptionID, next.PlanCode, next.Scope.Realm.Or())
		} else if err != nil {
			return fmt.Errorf("postgres: plan %s: %w", next.PlanCode, mapErr(err))
		}
		if _, err := tx.Exec(ctx, `INSERT INTO subscriptions (realm, scope_kind, scope_id, plan_id, provider_account, provider_subscription_id, status,
				current_period_start, current_period_end, cancel_at_period_end, grace_until, grace_lapsed_at, last_event_at, canceled_at)
			VALUES ($1,$2,$3,$4::uuid,$5,$6,$7::text,$8,$9,$10,$11,$12,$13::timestamptz, CASE WHEN $7::text = 'canceled' THEN $13::timestamptz END)
			ON CONFLICT (provider_account, provider_subscription_id) DO UPDATE SET plan_id = EXCLUDED.plan_id, status = EXCLUDED.status,
				current_period_start = EXCLUDED.current_period_start, current_period_end = EXCLUDED.current_period_end,
				cancel_at_period_end = EXCLUDED.cancel_at_period_end, grace_until = EXCLUDED.grace_until, grace_lapsed_at = EXCLUDED.grace_lapsed_at,
				last_event_at = EXCLUDED.last_event_at, updated_at = now(),
				canceled_at = CASE WHEN EXCLUDED.status = 'canceled' THEN coalesce(subscriptions.canceled_at, EXCLUDED.last_event_at) ELSE NULL END`,
			realmOf(next.Scope), next.Scope.Kind, next.Scope.ID, planID, next.Account, next.ProviderSubscriptionID, string(next.Status),
			optTime(next.CurrentPeriodStart), optTime(next.CurrentPeriodEnd), next.CancelAtPeriodEnd, next.GraceUntil, next.GraceLapsedAt, next.LastEventAt); err != nil {
			return fmt.Errorf("postgres: apply subscription %s: %w", next.ProviderSubscriptionID, mapErr(err))
		}
		applied = true
		return nil
	})
	return applied, err
}

// DueForGraceExpiry implements ports.Subs: past_due subscriptions whose grace window has ended and has not been
// announced.
func (s *Subs) DueForGraceExpiry(ctx context.Context, now time.Time) ([]domain.Sub, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+subColumns+subFrom+` WHERE s.status = 'past_due' AND s.grace_until IS NOT NULL AND s.grace_until <= $1
		AND s.grace_lapsed_at IS NULL ORDER BY s.grace_until, s.provider_subscription_id`, utc(now))
	if err != nil {
		return nil, fmt.Errorf("postgres: subscriptions past their grace: %w", mapErr(err))
	}
	defer rows.Close()
	var out []domain.Sub
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres: subscriptions past their grace: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// MarkGraceLapsed implements ports.Subs: it records the announcement only while it is still the grace that ended — a
// newer state clears the window or moves it, and then there is nothing to record.
func (s *Subs) MarkGraceLapsed(ctx context.Context, sub domain.Sub, at time.Time) (bool, error) {
	if sub.GraceUntil == nil {
		return false, nil
	}
	tag, err := s.pool.Exec(ctx, `UPDATE subscriptions SET grace_lapsed_at = $3, updated_at = now()
		WHERE provider_account = $1 AND provider_subscription_id = $2 AND status = 'past_due' AND grace_lapsed_at IS NULL AND grace_until = $4`,
		sub.Account, sub.ProviderSubscriptionID, utc(at), utc(*sub.GraceUntil))
	if err != nil {
		return false, fmt.Errorf("postgres: record the end of the grace of %s: %w", sub.ProviderSubscriptionID, mapErr(err))
	}
	return tag.RowsAffected() == 1, nil
}
