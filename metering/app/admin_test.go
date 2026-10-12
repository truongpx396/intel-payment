package app_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/metering"
	"github.com/truongpx396/intel-payment/metering/adapters/driven/pricing/table"
	"github.com/truongpx396/intel-payment/metering/app"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Configuration is data (constitution VIII), and this is its supported interface (FR-049): every write is
// validated, carries who made it and why, and a rate card is published only after it has been shown to
// price — a card that cannot is refused before it can become the card every Record is priced under.

// fakeConfig is a ConfigWriter that records what reached it.
type fakeConfig struct {
	mu      sync.Mutex
	writes  int
	cards   []domain.RateCard
	retire  []bool
	changes []ports.ConfigChange
	limits  []ports.LimitConfig
	pools   []ports.PoolConfig
	deleted []string
	err     error // returned by every call, when set
}

var _ ports.ConfigWriter = (*fakeConfig)(nil)

func (f *fakeConfig) record(ch ports.ConfigChange) {
	f.mu.Lock()
	f.writes++
	f.changes = append(f.changes, ch)
	f.mu.Unlock()
}

func (f *fakeConfig) PublishRateCard(_ context.Context, ch ports.ConfigChange, _ domain.Realm, c domain.RateCard, retire bool) (ports.RateCardInfo, error) {
	f.record(ch)
	if f.err != nil {
		return ports.RateCardInfo{}, f.err
	}
	f.mu.Lock()
	f.cards, f.retire = append(f.cards, c), append(f.retire, retire)
	f.mu.Unlock()
	return ports.RateCardInfo{Card: c}, nil
}

func (f *fakeConfig) RateCards(context.Context, domain.Realm) ([]ports.RateCardInfo, error) {
	return []ports.RateCardInfo{{Card: domain.RateCard{Version: "v1"}}}, f.err
}

func (f *fakeConfig) PutLimit(_ context.Context, ch ports.ConfigChange, _ domain.Realm, l ports.LimitConfig) (bool, error) {
	f.record(ch)
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	f.limits = append(f.limits, l)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeConfig) Limit(_ context.Context, _ domain.Realm, name string) (ports.LimitConfig, error) {
	if f.err != nil {
		return ports.LimitConfig{}, f.err
	}
	return ports.LimitConfig{Limit: domain.Limit{Name: name}}, nil
}

func (f *fakeConfig) DeleteLimit(_ context.Context, ch ports.ConfigChange, _ domain.Realm, name string) error {
	f.record(ch)
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	f.deleted = append(f.deleted, name)
	f.mu.Unlock()
	return nil
}

func (f *fakeConfig) PutPool(_ context.Context, ch ports.ConfigChange, _ domain.Realm, p ports.PoolConfig) (bool, error) {
	f.record(ch)
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	f.pools = append(f.pools, p)
	f.mu.Unlock()
	return true, nil
}

func (f *fakeConfig) Pool(_ context.Context, _ domain.Realm, name string) (ports.PoolConfig, error) {
	if f.err != nil {
		return ports.PoolConfig{}, f.err
	}
	return ports.PoolConfig{Pool: domain.PoolDef{Name: domain.Pool(name)}}, nil
}

// badPricer is a registered pricer with one defect, for the publication check to find.
type badPricer struct {
	price func(domain.RateCard, domain.Event) (domain.Price, error)
}

func (b badPricer) Price(_ context.Context, c domain.RateCard, e domain.Event) (domain.Price, error) {
	return b.price(c, e)
}

func newAdmin(t *testing.T, pricers ports.PricerRegistry) (*app.Admin, *fakeConfig) {
	t.Helper()
	if pricers == nil {
		pricers = ports.PricerRegistry{"table": table.New(0)}
	}
	store := &fakeConfig{}
	a, err := app.NewAdmin(metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}.WithDefaults(), pricers, store)
	if err != nil {
		t.Fatal(err)
	}
	return a, store
}

var (
	who      = ports.ConfigChange{Actor: "alice@example.com", Reason: "raise the free tier", Via: "ops-tool"}
	entries  = []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150_000, BlockSize: 1_000_000}, {RateKey: "gpt", Unit: "out", CreditsPerBlock: 600_000, BlockSize: 1_000_000}}
	goodCard = ports.CardPublication{Version: "2026-10-01", Entries: entries, RetirePrevious: true}
)

// Every write carries a person and a reason. The database audit row names both; a write that names neither is
// an audit row that cannot answer "who changed the price, and why".
func TestEveryConfigurationWriteNeedsAnActorAndAReason(t *testing.T) {
	t.Parallel()
	writes := map[string]func(*app.Admin, ports.ConfigChange) error{
		"publish a card": func(a *app.Admin, ch ports.ConfigChange) error {
			_, err := a.PublishRateCard(context.Background(), ch, "acme", goodCard)
			return err
		},
		"put a limit": func(a *app.Admin, ch ports.ConfigChange) error {
			_, err := a.PutLimit(context.Background(), ch, "acme", ports.LimitConfig{Limit: domain.Limit{Name: "user_daily", Unit: domain.CreditUnit, Max: 1, Window: domain.Daily, DenyCode: domain.DenyLimitReached}})
			return err
		},
		"delete a limit": func(a *app.Admin, ch ports.ConfigChange) error {
			return a.DeleteLimit(context.Background(), ch, "acme", "user_daily")
		},
		"put a pool": func(a *app.Admin, ch ports.ConfigChange) error {
			_, err := a.PutPool(context.Background(), ch, "acme", ports.PoolConfig{Pool: domain.PoolDef{Name: "promo", Priority: 10}})
			return err
		},
	}
	bad := map[string]ports.ConfigChange{
		"no actor":           {Reason: "because"},
		"a blank actor":      {Actor: "   ", Reason: "because"},
		"no reason":          {Actor: "alice"},
		"a blank reason":     {Actor: "alice", Reason: " \t\n"},
		"an enormous actor":  {Actor: strings.Repeat("a", 201), Reason: "because"},
		"an enormous reason": {Actor: "alice", Reason: strings.Repeat("r", 1001)},
	}
	for wname, write := range writes {
		for bname, ch := range bad {
			t.Run(wname+"/"+bname, func(t *testing.T) {
				t.Parallel()
				a, store := newAdmin(t, nil)
				err := write(a, ch)
				if !errors.Is(err, domain.ErrInvalid) {
					t.Fatalf("want ErrInvalid, got %v", err)
				}
				if !strings.Contains(err.Error(), "actor") && !strings.Contains(err.Error(), "reason") {
					t.Fatalf("the message should name what is missing: %v", err)
				}
				if store.writes != 0 {
					t.Fatal("a write reached the database without a person and a reason")
				}
			})
		}
		t.Run(wname+"/with both", func(t *testing.T) {
			t.Parallel()
			a, store := newAdmin(t, nil)
			if err := write(a, who); err != nil {
				t.Fatal(err)
			}
			if store.writes != 1 || store.changes[0] != who {
				t.Fatalf("the change reaches the store as given: %+v", store.changes)
			}
		})
	}
}

// FR-049: a card is published only after a test pricing.
func TestACardIsPublishedOnlyAfterItHasBeenShownToPrice(t *testing.T) {
	t.Parallel()

	t.Run("a good card is published, retiring the previous one as asked", func(t *testing.T) {
		t.Parallel()
		a, store := newAdmin(t, nil)
		info, err := a.PublishRateCard(context.Background(), who, "acme", goodCard)
		if err != nil {
			t.Fatal(err)
		}
		if len(store.cards) != 1 || !store.retire[0] || store.cards[0].Version != "2026-10-01" || store.cards[0].Pricer != "table" || store.cards[0].Realm != "acme" || len(store.cards[0].Entries) != 2 || info.Card.Version != "2026-10-01" {
			t.Fatalf("%+v %+v", store.cards, info)
		}
		keep := goodCard
		keep.RetirePrevious = false
		if _, err := a.PublishRateCard(context.Background(), who, "acme", keep); err != nil || store.retire[1] {
			t.Fatalf("retire_previous false is honoured: %v %v", err, store.retire)
		}
	})

	refused := map[string]struct {
		pub     ports.CardPublication
		pricers ports.PricerRegistry
		want    error
	}{
		"a pricer nobody registered":          {ports.CardPublication{Version: "v2", Pricer: "llmtoken-v9", Entries: entries}, nil, domain.ErrUnpriceable},
		"a price that overflows":              {ports.CardPublication{Version: "v2", Entries: []domain.RateEntry{{RateKey: "gpu", Unit: "s", CreditsPerBlock: 1 << 62, BlockSize: 1}}}, nil, domain.ErrAmountOutOfRange},
		"a price above the maximum operation": {ports.CardPublication{Version: "v2", Entries: []domain.RateEntry{{RateKey: "gpu", Unit: "s", CreditsPerBlock: int64(domain.MaxOperationAmount), BlockSize: 1}}}, nil, domain.ErrAmountOutOfRange},
		"a card that prices nothing":          {ports.CardPublication{Version: "v2"}, nil, domain.ErrInvalid},
		"no version":                          {ports.CardPublication{Entries: entries}, nil, domain.ErrInvalid},
		"a version with a space":              {ports.CardPublication{Version: "v 2", Entries: entries}, nil, domain.ErrInvalid},
		"an enormous version":                 {ports.CardPublication{Version: strings.Repeat("v", 65), Entries: entries}, nil, domain.ErrInvalid},
		"a negative price":                    {ports.CardPublication{Version: "v2", Entries: []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: -1, BlockSize: 1}}}, nil, domain.ErrInvalid},
		"a zero block":                        {ports.CardPublication{Version: "v2", Entries: []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 1, BlockSize: 0}}}, nil, domain.ErrInvalid},
		"two prices for one thing":            {ports.CardPublication{Version: "v2", Entries: append(append([]domain.RateEntry{}, entries...), entries[0])}, nil, domain.ErrInvalid},
		"a compiled pricer that prices negative": {
			ports.CardPublication{Version: "v2", Pricer: "negative", Entries: entries},
			ports.PricerRegistry{"table": table.New(0), "negative": badPricer{func(c domain.RateCard, _ domain.Event) (domain.Price, error) {
				return domain.Price{Credits: -5, RateCardVersion: c.Version}, nil
			}}}, domain.ErrUnpriceable},
		"a compiled pricer that errors": {
			ports.CardPublication{Version: "v2", Pricer: "broken", Entries: entries},
			ports.PricerRegistry{"table": table.New(0), "broken": badPricer{func(domain.RateCard, domain.Event) (domain.Price, error) {
				return domain.Price{}, domain.ErrUnpriceable
			}}}, domain.ErrUnpriceable},
		"a compiled pricer that exceeds the maximum": {
			ports.CardPublication{Version: "v2", Pricer: "greedy", Entries: entries},
			ports.PricerRegistry{"table": table.New(0), "greedy": badPricer{func(c domain.RateCard, _ domain.Event) (domain.Price, error) {
				return domain.Price{Credits: domain.MaxOperationAmount + 1, RateCardVersion: c.Version}, nil
			}}}, domain.ErrAmountOutOfRange},
	}
	for name, tc := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			t.Parallel()
			a, store := newAdmin(t, tc.pricers)
			_, err := a.PublishRateCard(context.Background(), who, "acme", tc.pub)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if store.writes != 0 {
				t.Fatal("a card that cannot price reached the database: it would have become the card every Record is priced under")
			}
		})
	}

	// One bad entry among good ones: the test pricing covers every rate key and unit, not a sample.
	t.Run("refused: a defect in the last entry only", func(t *testing.T) {
		t.Parallel()
		picky := badPricer{func(c domain.RateCard, e domain.Event) (domain.Price, error) {
			for _, q := range e.Quantities {
				if e.RateKey == "z" && q.Unit == "last" {
					return domain.Price{}, domain.ErrUnpriceable
				}
			}
			return domain.Price{Credits: 1, RateCardVersion: c.Version}, nil
		}}
		a, store := newAdmin(t, ports.PricerRegistry{"table": table.New(0), "picky": picky})
		pub := ports.CardPublication{Version: "v2", Pricer: "picky", Entries: []domain.RateEntry{
			{RateKey: "a", Unit: "u", CreditsPerBlock: 1, BlockSize: 1}, {RateKey: "b", Unit: "u", CreditsPerBlock: 1, BlockSize: 1},
			{RateKey: "z", Unit: "last", CreditsPerBlock: 1, BlockSize: 1},
		}}
		if _, err := a.PublishRateCard(context.Background(), who, "acme", pub); !errors.Is(err, domain.ErrUnpriceable) || store.writes != 0 {
			t.Fatalf("%v (writes %d)", err, store.writes)
		}
	})

	t.Run("a card is shown to price an event of its own size AND a large one", func(t *testing.T) {
		t.Parallel()
		// 1 unit of this entry costs 1 credit — fine — but a thousand blocks of it overflow the maximum.
		a, store := newAdmin(t, nil)
		pub := ports.CardPublication{Version: "v2", Entries: []domain.RateEntry{{RateKey: "gpu", Unit: "s", CreditsPerBlock: int64(domain.MaxOperationAmount) / 100, BlockSize: 1}}}
		if _, err := a.PublishRateCard(context.Background(), who, "acme", pub); !errors.Is(err, domain.ErrAmountOutOfRange) || store.writes != 0 {
			t.Fatalf("%v (writes %d)", err, store.writes)
		}
	})

	t.Run("a store that says the version exists is passed through, not hidden", func(t *testing.T) {
		t.Parallel()
		a, store := newAdmin(t, nil)
		store.err = domain.ErrAlreadyExists
		if _, err := a.PublishRateCard(context.Background(), who, "acme", goodCard); !errors.Is(err, domain.ErrAlreadyExists) {
			t.Fatalf("%v", err)
		}
	})
}

func TestALimitIsValidatedBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	good := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, TZ: "UTC", WarnAt: 0.8, DenyCode: domain.DenyLimitReached}
	two := 20
	t.Run("a good one is written, with its sort order when given", func(t *testing.T) {
		t.Parallel()
		a, store := newAdmin(t, nil)
		created, err := a.PutLimit(context.Background(), who, "acme", ports.LimitConfig{Limit: good, SortOrder: &two})
		if err != nil || !created || len(store.limits) != 1 || store.limits[0].Limit.Name != "user_daily" || *store.limits[0].SortOrder != 20 {
			t.Fatalf("%v %v %+v", created, err, store.limits)
		}
	})
	bad := map[string]ports.LimitConfig{
		"no name":                          {Limit: func() domain.Limit { l := good; l.Name = ""; return l }()},
		"a name that is not a slug":        {Limit: func() domain.Limit { l := good; l.Name = "User Daily!"; return l }()},
		"a negative max":                   {Limit: func() domain.Limit { l := good; l.Max = -1; return l }()},
		"a rolling limit with no duration": {Limit: domain.Limit{Name: "r", Unit: domain.CreditUnit, Max: 1, Window: domain.Rolling, DenyCode: domain.DenyLimitReached}},
		"a zone nobody has":                {Limit: func() domain.Limit { l := good; l.TZ = "Mars/Olympus_Mons"; return l }()},
		"an unknown deny code":             {Limit: func() domain.Limit { l := good; l.DenyCode = "nope"; return l }()},
		"a warning past 100%":              {Limit: func() domain.Limit { l := good; l.WarnAt = 1.5; return l }()},
		"a balance limit on a subject":     {Limit: domain.Limit{Name: "b", Subject: "user", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}},
		"a negative sort order":            {Limit: good, SortOrder: func() *int { n := -1; return &n }()},
	}
	for name, l := range bad {
		t.Run("refused: "+name, func(t *testing.T) {
			t.Parallel()
			a, store := newAdmin(t, nil)
			if _, err := a.PutLimit(context.Background(), who, "acme", l); !errors.Is(err, domain.ErrInvalid) || store.writes != 0 {
				t.Fatalf("%v (writes %d)", err, store.writes)
			}
		})
	}
	t.Run("deleting names a limit that could exist", func(t *testing.T) {
		t.Parallel()
		a, store := newAdmin(t, nil)
		if err := a.DeleteLimit(context.Background(), who, "acme", "User Daily!"); !errors.Is(err, domain.ErrInvalid) || store.writes != 0 {
			t.Fatalf("%v", err)
		}
		if err := a.DeleteLimit(context.Background(), who, "acme", "user_daily"); err != nil || len(store.deleted) != 1 {
			t.Fatalf("%v", err)
		}
	})
}

func TestAPoolIsValidatedBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	t.Run("a good one", func(t *testing.T) {
		t.Parallel()
		a, store := newAdmin(t, nil)
		if _, err := a.PutPool(context.Background(), who, "acme", ports.PoolConfig{Pool: domain.PoolDef{Name: "gpu", Priority: 5, AppliesTo: []string{"gpu.job"}}, Description: "gpu credits"}); err != nil || len(store.pools) != 1 || store.pools[0].Pool.Priority != 5 {
			t.Fatalf("%v %+v", err, store.pools)
		}
	})
	for name, p := range map[string]ports.PoolConfig{
		"general is implicit and cannot be configured": {Pool: domain.PoolDef{Name: domain.GeneralPool, Priority: 1}},
		"no name":                   {Pool: domain.PoolDef{Priority: 1}},
		"a name that is not a slug": {Pool: domain.PoolDef{Name: "Promo Pool", Priority: 1}},
		"a resource that is empty":  {Pool: domain.PoolDef{Name: "gpu", Priority: 1, AppliesTo: []string{"gpu.job", ""}}},
	} {
		t.Run("refused: "+name, func(t *testing.T) {
			t.Parallel()
			a, store := newAdmin(t, nil)
			if _, err := a.PutPool(context.Background(), who, "acme", p); !errors.Is(err, domain.ErrInvalid) || store.writes != 0 {
				t.Fatalf("%v (writes %d)", err, store.writes)
			}
		})
	}
}

func TestReadsPassThroughAndKeepTheirErrors(t *testing.T) {
	t.Parallel()
	a, store := newAdmin(t, nil)
	if cards, err := a.RateCards(context.Background(), "acme"); err != nil || len(cards) != 1 {
		t.Fatalf("%v %v", cards, err)
	}
	if l, err := a.Limit(context.Background(), "acme", "user_daily"); err != nil || l.Limit.Name != "user_daily" {
		t.Fatalf("%v %v", l, err)
	}
	store.err = domain.ErrNotFound
	if _, err := a.Limit(context.Background(), "acme", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := a.Pool(context.Background(), "acme", "nope"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("%v", err)
	}
	if _, err := a.Limit(context.Background(), "acme", "Bad Name"); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("a name that cannot exist is invalid, not missing: %v", err)
	}
}

func TestNewAdminNeedsItsParts(t *testing.T) {
	t.Parallel()
	cfg := metering.Config{BalanceRedisURL: "redis://x", LedgerDSN: "postgres://x"}.WithDefaults()
	if _, err := app.NewAdmin(cfg, ports.PricerRegistry{"table": table.New(0)}, nil); err == nil {
		t.Fatal("no store")
	}
	if _, err := app.NewAdmin(cfg, ports.PricerRegistry{}, &fakeConfig{}); err == nil {
		t.Fatal("no table pricer: every deployment has one, and a card that names it could not be tested")
	}
	if _, err := app.NewAdmin(metering.Config{}, ports.PricerRegistry{"table": table.New(0)}, &fakeConfig{}); err == nil {
		t.Fatal("an invalid config")
	}
}
