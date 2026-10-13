package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Admin is the administrative interface to configuration (FR-049): the rules, and then the store. Every write
// names a person and a reason; a card is published only after it has been shown to price; a limit or pool is
// validated before it is written. Whatever the rules let through is audited by the database itself, in the same
// transaction — including a change made by bypassing this API.
type Admin struct {
	cfg     metering.Config
	pricers ports.PricerRegistry
	store   ports.ConfigWriter
}

var _ ports.Admin = (*Admin)(nil)

// NewAdmin returns the admin service. Every deployment has the `table` pricer, and without it a card that names
// it could not be tested.
func NewAdmin(cfg metering.Config, pricers ports.PricerRegistry, store ports.ConfigWriter) (*Admin, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("metering config: %w", err)
	}
	var errs []error
	if store == nil {
		errs = append(errs, errors.New("a ConfigWriter is required"))
	}
	if pricers["table"] == nil {
		errs = append(errs, errors.New("the `table` pricer must be registered: a card that names it could not be test-priced"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("metering admin: %w", err)
	}
	return &Admin{cfg: cfg.WithDefaults(), pricers: pricers, store: store}, nil
}

const (
	maxActor  = 200
	maxReason = 1000
)

var (
	versionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	limitPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// checkChange requires a person and a reason. The audit row is how anyone later answers "who changed the
// price, and why", and a row that names neither cannot.
func checkChange(ch ports.ConfigChange) error {
	switch {
	case strings.TrimSpace(ch.Actor) == "":
		return fmt.Errorf("%w: actor is required: the audit row names who made the change", domain.ErrInvalid)
	case strings.TrimSpace(ch.Reason) == "":
		return fmt.Errorf("%w: reason is required: the audit row says why the change was made", domain.ErrInvalid)
	case len(ch.Actor) > maxActor:
		return fmt.Errorf("%w: actor is over %d bytes", domain.ErrInvalid, maxActor)
	case len(ch.Reason) > maxReason:
		return fmt.Errorf("%w: reason is over %d bytes", domain.ErrInvalid, maxReason)
	}
	return nil
}

func checkRealm(r domain.Realm) (domain.Realm, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return r.Or(), nil
}

// A card is test-priced per entry at one unit and at a thousand blocks. The last is what finds a price that is
// fine for one call and overflows for a busy one; for a pricer that grows with the quantity it also covers every
// size in between.
const testBlocks = 1000

// PublishRateCard validates a card and publishes it. The test pricing runs the card's own pricer over every
// (rate key, unit), so one bad entry cannot hide among good ones.
func (a *Admin) PublishRateCard(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, p ports.CardPublication) (ports.RateCardInfo, error) {
	if err := checkChange(ch); err != nil {
		return ports.RateCardInfo{}, err
	}
	realm, err := checkRealm(realm)
	if err != nil {
		return ports.RateCardInfo{}, err
	}
	if !versionPattern.MatchString(p.Version) {
		return ports.RateCardInfo{}, fmt.Errorf("%w: a card version is 1–64 letters, digits, '.', '_' or '-', got %q", domain.ErrInvalid, p.Version)
	}
	if len(p.Entries) == 0 {
		return ports.RateCardInfo{}, fmt.Errorf("%w: card %q prices nothing", domain.ErrInvalid, p.Version)
	}
	card, err := domain.NewRateCard(realm, p.Version, p.Pricer, p.Entries, nil)
	if err != nil {
		return ports.RateCardInfo{}, err
	}
	pricer := a.pricers[card.Pricer]
	if pricer == nil {
		return ports.RateCardInfo{}, fmt.Errorf("%w: card %q names pricer %q, which is not registered", domain.ErrUnpriceable, card.Version, card.Pricer)
	}
	if err := a.testPrice(ctx, pricer, card); err != nil {
		return ports.RateCardInfo{}, err
	}
	return a.store.PublishRateCard(ctx, ch, realm, card, p.RetirePrevious)
}

func (a *Admin) testPrice(ctx context.Context, pricer ports.Pricer, card domain.RateCard) error {
	for _, e := range card.Entries {
		big := e.BlockSize // a block this large cannot be multiplied by a thousand without wrapping
		if e.BlockSize <= (1<<62)/testBlocks {
			big = e.BlockSize * testBlocks
		}
		for _, n := range []int64{1, big} {
			ev := domain.Event{RateKey: e.RateKey, Quantities: []domain.Quantity{{Unit: e.Unit, Amount: n}}}
			price, err := pricer.Price(ctx, card, ev)
			where := fmt.Sprintf("card %q fails its test pricing of %d %s of %s", card.Version, n, e.Unit, e.RateKey)
			switch {
			case err != nil:
				return fmt.Errorf("%s: %w", where, err)
			case price.Credits < 0:
				return fmt.Errorf("%w: %s: it prices negative (%d credits)", domain.ErrUnpriceable, where, price.Credits)
			case price.Credits > a.cfg.MaxOperationAmount:
				return fmt.Errorf("%w: %s: %d credits is above the maximum operation (%d)", domain.ErrAmountOutOfRange, where, price.Credits, a.cfg.MaxOperationAmount)
			}
		}
	}
	return nil
}

// RateCards lists the realm's cards, retired ones included.
func (a *Admin) RateCards(ctx context.Context, realm domain.Realm) ([]ports.RateCardInfo, error) {
	realm, err := checkRealm(realm)
	if err != nil {
		return nil, err
	}
	return a.store.RateCards(ctx, realm)
}

func checkLimitName(name string) error {
	if !limitPattern.MatchString(name) {
		return fmt.Errorf("%w: a limit name is a lowercase slug (letters, digits, '_'), got %q", domain.ErrInvalid, name)
	}
	return nil
}

// PutLimit validates a ceiling and writes it. It takes effect on every process at once: the database's own
// change notification invalidates their caches.
func (a *Admin) PutLimit(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, l ports.LimitConfig) (bool, error) {
	if err := checkChange(ch); err != nil {
		return false, err
	}
	realm, err := checkRealm(realm)
	if err != nil {
		return false, err
	}
	if err := checkLimitName(l.Limit.Name); err != nil {
		return false, err
	}
	if err := l.Limit.Validate(); err != nil {
		return false, err
	}
	if l.SortOrder != nil && *l.SortOrder < 0 {
		return false, fmt.Errorf("%w: sort_order must be >= 0", domain.ErrInvalid)
	}
	return a.store.PutLimit(ctx, ch, realm, l)
}

// Limit reads one ceiling by name.
func (a *Admin) Limit(ctx context.Context, realm domain.Realm, name string) (ports.LimitConfig, error) {
	realm, err := checkRealm(realm)
	if err != nil {
		return ports.LimitConfig{}, err
	}
	if err := checkLimitName(name); err != nil {
		return ports.LimitConfig{}, err
	}
	return a.store.Limit(ctx, realm, name)
}

// DeleteLimit removes a ceiling.
func (a *Admin) DeleteLimit(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, name string) error {
	if err := checkChange(ch); err != nil {
		return err
	}
	realm, err := checkRealm(realm)
	if err != nil {
		return err
	}
	if err := checkLimitName(name); err != nil {
		return err
	}
	return a.store.DeleteLimit(ctx, ch, realm, name)
}

func checkPool(p ports.PoolConfig) error {
	switch p.Pool.Name {
	case "":
		return fmt.Errorf("%w: a pool needs a name", domain.ErrInvalid)
	case domain.GeneralPool:
		return fmt.Errorf("%w: %q is implicit in every realm and cannot be configured", domain.ErrInvalid, domain.GeneralPool)
	}
	if err := p.Pool.Name.Validate(); err != nil {
		return err
	}
	for _, r := range p.Pool.AppliesTo {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("%w: applies_to names an empty resource", domain.ErrInvalid)
		}
	}
	return nil
}

// PutPool validates a pool and writes it.
func (a *Admin) PutPool(ctx context.Context, ch ports.ConfigChange, realm domain.Realm, p ports.PoolConfig) (bool, error) {
	if err := checkChange(ch); err != nil {
		return false, err
	}
	realm, err := checkRealm(realm)
	if err != nil {
		return false, err
	}
	if err := checkPool(p); err != nil {
		return false, err
	}
	return a.store.PutPool(ctx, ch, realm, p)
}

// Pool reads one pool by name.
func (a *Admin) Pool(ctx context.Context, realm domain.Realm, name string) (ports.PoolConfig, error) {
	realm, err := checkRealm(realm)
	if err != nil {
		return ports.PoolConfig{}, err
	}
	if err := domain.Pool(name).Validate(); err != nil || name == "" {
		return ports.PoolConfig{}, fmt.Errorf("%w: %q is not a pool name", domain.ErrInvalid, name)
	}
	return a.store.Pool(ctx, realm, name)
}
