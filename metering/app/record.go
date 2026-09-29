package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Record prices e under the realm's active card and settles it idempotently (FR-008, FR-017e).
// Pass the quantities ACTUALLY produced — partial on cancel (invariant 4).
//
// It refuses a stale OccurredAt, and when the hot tier cannot take the charge — or
// Settlement=journal — the event is written to the spend journal first and Receipt.Deferred is
// returned, so no usage is ever dropped by an outage (invariant 16).
func (m *meter) Record(ctx context.Context, e domain.Event) (domain.Receipt, error) {
	if err := e.Validate(); err != nil {
		return domain.Receipt{}, err
	}
	e.Scope = e.Scope.Normalized()

	if err := m.checkFresh(e); err != nil {
		return domain.Receipt{}, err
	}
	price, err := m.price(ctx, e)
	if err != nil {
		return domain.Receipt{}, err
	}
	c := m.charge(e, price)
	entry := ports.JournalEntry{Charge: c, Fingerprint: c.Fingerprint()}

	// journal settlement: Postgres is the intent's FIRST durable home; then the hot function.
	journaled := false
	if m.cfg.Settlement == metering.SettlementJournal {
		if err := m.journal(ctx, entry); err != nil {
			return domain.Receipt{}, err
		}
		journaled = true
	}

	var rc domain.Receipt
	err = m.reh.heal(ctx, e.Scope, func() (e error) { rc, e = m.d.Balance.Debit(ctx, c); return })
	switch {
	case err == nil:
		rc.RateCardVersion = price.RateCardVersion
		return rc, nil
	case errors.Is(err, domain.ErrIdemConflict):
		m.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(e.Scope.Realm), "op": "usage"})
		return domain.Receipt{}, err
	case domain.IsHotUnavailable(err):
		// An outage. The charge is not lost: it goes to the journal, and the replay tick applies it
		// through the same hot function — idempotent on the same key — once the shard is live.
		if !journaled {
			if jerr := m.journal(ctx, entry); jerr != nil {
				// Failed at BOTH stores. The host sees an error and retries; nothing was accepted.
				return domain.Receipt{}, fmt.Errorf("%w: and the journal is unavailable too: %w", err, jerr)
			}
		}
		m.met.Count(ports.MetricRecordDeferred, 1, realmLabel(e.Scope))
		return domain.Receipt{IdemKey: e.IdemKey, Applied: true, Deferred: true, RateCardVersion: price.RateCardVersion}, nil
	}
	return domain.Receipt{}, err
}

// checkFresh refuses an event whose durable guard may already have expired (a second charge waiting
// to happen) or whose clock is in the future.
func (m *meter) checkFresh(e domain.Event) error {
	now := m.d.Clock.Now()
	if age := now.Sub(e.OccurredAt); age > m.cfg.UsageIdemWindow-m.cfg.IdemSafetyMargin {
		m.met.Count(ports.MetricStaleEvent, 1, realmLabel(e.Scope))
		return fmt.Errorf("%w: occurred %s ago, beyond %s", domain.ErrStaleEvent, age.Round(1e9), m.cfg.UsageIdemWindow-m.cfg.IdemSafetyMargin)
	}
	if skew := e.OccurredAt.Sub(now); skew > m.cfg.MaxClockSkew {
		return fmt.Errorf("%w: occurred_at is %s in the future (max clock skew %s)", domain.ErrInvalid, skew.Round(1e9), m.cfg.MaxClockSkew)
	}
	return nil
}

// price resolves the realm's active card and its pricer, and prices the event. Every failure here
// fails CLOSED and is counted: pricing at zero, or guessing, would under-bill silently.
func (m *meter) price(ctx context.Context, e domain.Event) (domain.Price, error) {
	fail := func(reason string, err error) (domain.Price, error) {
		m.met.Count(ports.MetricPriceError, 1, ports.Labels{"realm": string(e.Scope.Realm), "rate_key": e.RateKey, "reason": reason})
		return domain.Price{}, err
	}
	card, err := m.d.RateCards.Active(ctx, e.Scope.Realm)
	if err != nil {
		return fail("no_card", err)
	}
	p, ok := m.d.Pricers[card.Pricer]
	if !ok || p == nil {
		return fail("no_pricer", fmt.Errorf("%w: card %q names pricer %q, which is not registered", domain.ErrUnpriceable, card.Version, card.Pricer))
	}
	price, err := p.Price(ctx, card, e)
	if err != nil {
		reason := "unpriceable"
		if errors.Is(err, domain.ErrAmountOutOfRange) {
			reason = "out_of_range"
		}
		return fail(reason, err)
	}
	// A compiled pricer is untrusted with the range check the table pricer does for itself.
	if price.Credits < 0 || price.Credits > m.cfg.MaxOperationAmount {
		return fail("out_of_range", fmt.Errorf("%w: %d credits", domain.ErrAmountOutOfRange, price.Credits))
	}
	if price.RateCardVersion == "" {
		price.RateCardVersion = card.Version // the row must name its card (invariant 5)
	}
	return price, nil
}

// charge builds the hot-tier charge from an event and its price. The ledger row's operation_type is
// the event's `operation_type` attribute when the host sets one, and its resource otherwise.
func (m *meter) charge(e domain.Event, p domain.Price) domain.Charge {
	reason := e.Attributes["operation_type"]
	if reason == "" {
		reason = e.Resource
	}
	ref := e.Attributes
	if ref != nil {
		ref = make(map[string]string, len(e.Attributes))
		for k, v := range e.Attributes {
			if k != "operation_type" {
				ref[k] = v
			}
		}
	}
	return domain.Charge{
		Scope: e.Scope, Amount: p.Credits, IdemKey: e.IdemKey, Reason: reason, Resource: e.Resource,
		Subjects: e.Subjects, Quantities: e.Quantities, Ref: ref,
		RateKey: e.RateKey, RateCardVersion: p.RateCardVersion, CostMicros: p.CostMicros, OccurredAt: e.OccurredAt,
	}
}

// journal records the charge durably. An identical retry is fine; the same key with a different
// request is ErrIdemConflict.
func (m *meter) journal(ctx context.Context, e ports.JournalEntry) error {
	if _, err := m.d.Journal.Put(ctx, e); err != nil {
		if errors.Is(err, domain.ErrIdemConflict) {
			m.met.Count(ports.MetricIdemConflict, 1, ports.Labels{"realm": string(e.Charge.Scope.Realm), "op": "usage"})
		}
		return err
	}
	return nil
}
