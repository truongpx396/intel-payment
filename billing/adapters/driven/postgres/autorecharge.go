package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// AutoRecharges are the standing instructions to top up, and the ledger that bounds them (payments invariant 18). The
// rules are the domain's — Claim and Settle are pure — and this store only makes them atomic: each runs on the instruction's
// row under FOR UPDATE, so however many triggers fire at once they are applied one after the other, each to what the one
// before it left.
//
// An instruction IS a balance watch. Metering owns the mechanism (the hot function flags a crossing of a watch, the writer
// publishes billing.balance.low) and billing owns what it triggers; so the instruction keeps a watch of key
// auto_recharge in balance_watches, in the same transaction, and never touches a watch of any other key.
type AutoRecharges struct{ pool *pgxpool.Pool }

var _ ports.AutoRecharges = (*AutoRecharges)(nil)

// NewAutoRecharges returns the instructions of a database.
func NewAutoRecharges(pool *pgxpool.Pool) *AutoRecharges { return &AutoRecharges{pool: pool} }

// WatchKey is the key of the balance watch an instruction keeps.
const WatchKey = "auto_recharge"

const rechargeColumns = `realm, scope_kind, scope_id, enabled, pool, threshold, plan_code, currency, provider_account, payment_method_ref,
	max_per_day, last_triggered_at, consecutive_failures, coalesce(disabled_reason, ''), coalesce(to_char(day, 'YYYY-MM-DD'), ''),
	charges_today, inflight_n, inflight_since`

func scanRecharge(row pgx.Row) (domain.AutoRecharge, error) {
	var a domain.AutoRecharge
	var realm, pool, currency string
	var last, since *time.Time
	if err := row.Scan(&realm, &a.Scope.Kind, &a.Scope.ID, &a.Enabled, &pool, &a.Threshold, &a.PlanCode, &currency, &a.Account, &a.PaymentMethodRef,
		&a.MaxPerDay, &last, &a.ConsecutiveFailed, &a.DisabledReason, &a.Day, &a.ChargesToday, &a.InflightN, &since); err != nil {
		return domain.AutoRecharge{}, err
	}
	a.Scope.Realm, a.Pool, a.Currency = domain.Realm(realm), domain.Pool(pool), strings.TrimSpace(currency)
	a.LastTriggeredAt, a.InflightSince = utcPtr(last), utcPtr(since)
	return a, nil
}

// Get implements ports.AutoRecharges.
func (s *AutoRecharges) Get(ctx context.Context, sc domain.Scope) (domain.AutoRecharge, bool, error) {
	a, err := scanRecharge(s.pool.QueryRow(ctx, `SELECT `+rechargeColumns+` FROM auto_recharge WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`,
		realmOf(sc), sc.Kind, sc.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.AutoRecharge{}, false, nil
	case err != nil:
		return domain.AutoRecharge{}, false, fmt.Errorf("postgres: auto top-up of %s: %w", sc.Tag(), mapErr(err))
	}
	return a, true, nil
}

// Put implements ports.AutoRecharges: the configuration is stored and the day's ledger is not touched, so editing an
// instruction can never hand out more charges today. Writing it enabled clears its failures (its owner has fixed the
// card); writing it disabled records the reason given. The balance watch follows.
func (s *AutoRecharges) Put(ctx context.Context, a domain.AutoRecharge) error {
	if err := a.Validate(); err != nil {
		return err
	}
	reason := a.DisabledReason
	if a.Enabled {
		reason = ""
	} else if reason == "" {
		reason = "disabled"
	}
	return inTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockKey(ctx, tx, "autorecharge:"+a.Scope.Tag()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO auto_recharge (realm, scope_kind, scope_id, enabled, pool, threshold, plan_code, currency, provider_account,
				payment_method_ref, max_per_day, disabled_reason)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (realm, scope_kind, scope_id) DO UPDATE SET enabled = EXCLUDED.enabled, pool = EXCLUDED.pool, threshold = EXCLUDED.threshold,
				plan_code = EXCLUDED.plan_code, currency = EXCLUDED.currency, provider_account = EXCLUDED.provider_account,
				payment_method_ref = EXCLUDED.payment_method_ref, max_per_day = EXCLUDED.max_per_day,
				consecutive_failures = CASE WHEN EXCLUDED.enabled THEN 0 ELSE auto_recharge.consecutive_failures END,
				disabled_reason = EXCLUDED.disabled_reason, updated_at = now()`,
			realmOf(a.Scope), a.Scope.Kind, a.Scope.ID, a.Enabled, string(a.Pool.Or()), a.Threshold, a.PlanCode, a.Currency, a.Account,
			a.PaymentMethodRef, a.MaxPerDay, nilIfEmpty(reason))
		if err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == codeForeignKey {
				return fmt.Errorf("%w: auto top-up of %s charges provider account %q, which does not exist", md.ErrInvalid, a.Scope.Tag(), a.Account)
			}
			return fmt.Errorf("postgres: store the auto top-up of %s: %w", a.Scope.Tag(), mapErr(err))
		}
		return syncWatch(ctx, tx, a.Scope, a.Enabled, a.Pool.Or(), a.Threshold)
	})
}

// syncWatch makes the scope's auto_recharge watch what the instruction says: one watch while it is enabled, none while it
// is not. Watches of other keys are the host's and are never touched.
func syncWatch(ctx context.Context, tx pgx.Tx, sc domain.Scope, enabled bool, pool domain.Pool, threshold int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM balance_watches WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 AND key = $4`,
		realmOf(sc), sc.Kind, sc.ID, WatchKey); err != nil {
		return fmt.Errorf("postgres: clear the balance watch of %s: %w", sc.Tag(), mapErr(err))
	}
	if !enabled {
		return nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO balance_watches (realm, scope_kind, scope_id, pool, key, threshold) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (realm, scope_kind, scope_id, pool, key) DO UPDATE SET threshold = EXCLUDED.threshold`,
		realmOf(sc), sc.Kind, sc.ID, string(pool), WatchKey, threshold); err != nil {
		return fmt.Errorf("postgres: set the balance watch of %s: %w", sc.Tag(), mapErr(err))
	}
	return nil
}

// locked reads the instruction under FOR UPDATE, so the claim or settlement that follows is applied to what the one before
// it left.
func locked(ctx context.Context, tx pgx.Tx, sc domain.Scope) (domain.AutoRecharge, bool, error) {
	a, err := scanRecharge(tx.QueryRow(ctx, `SELECT `+rechargeColumns+` FROM auto_recharge WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3 FOR UPDATE`,
		realmOf(sc), sc.Kind, sc.ID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return domain.AutoRecharge{}, false, nil
	case err != nil:
		return domain.AutoRecharge{}, false, fmt.Errorf("postgres: auto top-up of %s: %w", sc.Tag(), mapErr(err))
	}
	return a, true, nil
}

// Claim implements ports.AutoRecharges: the domain's Claim, applied under the instruction's row lock. A scope with no
// instruction has nothing to claim, which is the same as a disabled one.
func (s *AutoRecharges) Claim(ctx context.Context, sc domain.Scope, now time.Time, staleAfter time.Duration) (ports.RechargeClaim, ports.RechargeRefusal, error) {
	var claim ports.RechargeClaim
	refusal := ports.RechargeDisabled
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, found, err := locked(ctx, tx, sc)
		if err != nil || !found {
			return err
		}
		next, c, r := cur.Claim(now, staleAfter)
		claim, refusal = c, r
		if r != ports.RechargeOK {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE auto_recharge SET day = $4::date, charges_today = $5, inflight_n = $6, inflight_since = $7,
				last_triggered_at = $8, updated_at = now() WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`,
			realmOf(sc), sc.Kind, sc.ID, next.Day, next.ChargesToday, next.InflightN, utcPtr(next.InflightSince), utcPtr(next.LastTriggeredAt)); err != nil {
			return fmt.Errorf("postgres: claim a slot for %s: %w", sc.Tag(), mapErr(err))
		}
		return nil
	})
	if err != nil {
		return ports.RechargeClaim{}, "", err
	}
	return claim, refusal, nil
}

// Settle implements ports.AutoRecharges: the domain's Settle, applied under the instruction's row lock. When it switches
// the instruction off, its balance watch goes with it.
func (s *AutoRecharges) Settle(ctx context.Context, sc domain.Scope, slot ports.RechargeClaim, ok bool, reason string, now time.Time) (bool, error) {
	var disabled bool
	err := inTx(ctx, s.pool, func(tx pgx.Tx) error {
		cur, found, err := locked(ctx, tx, sc)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: no auto top-up for %s", md.ErrNotFound, sc.Tag())
		}
		next, off := cur.Settle(slot, ok, reason)
		disabled = off
		if _, err := tx.Exec(ctx, `UPDATE auto_recharge SET enabled = $4, consecutive_failures = $5, disabled_reason = $6, inflight_n = $7, inflight_since = $8,
				updated_at = $9 WHERE realm = $1 AND scope_kind = $2 AND scope_id = $3`,
			realmOf(sc), sc.Kind, sc.ID, next.Enabled, next.ConsecutiveFailed, nilIfEmpty(next.DisabledReason), next.InflightN, utcPtr(next.InflightSince), utc(now)); err != nil {
			return fmt.Errorf("postgres: settle the slot of %s: %w", sc.Tag(), mapErr(err))
		}
		if off {
			return syncWatch(ctx, tx, sc, false, cur.Pool.Or(), cur.Threshold)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return disabled, nil
}
