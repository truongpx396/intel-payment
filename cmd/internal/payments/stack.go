package payments

import (
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	pgbilling "github.com/truongpx396/intel-payment/billing/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/billing/adapters/driving/paymenthttp"
	"github.com/truongpx396/intel-payment/billing/app"
	"github.com/truongpx396/intel-payment/billing/domain"
	"github.com/truongpx396/intel-payment/billing/ports"
	"github.com/truongpx396/intel-payment/cmd/internal/envcfg"
	mports "github.com/truongpx396/intel-payment/metering/ports"
)

// Books are the payment layer's stores over one database.
type Books struct {
	Accounts  *pgbilling.Accounts
	Customers *pgbilling.Customers
	Catalog   *pgbilling.Catalog
	Payments  *pgbilling.Payments
	Subs      *pgbilling.Subs
	Inbox     *pgbilling.Inbox
	Recharges *pgbilling.AutoRecharges
	Config    *pgbilling.ConfigWriter
}

// OpenBooks returns the stores of a database. Opening them dials nothing: the pool is the caller's.
func OpenBooks(pool *pgxpool.Pool, clock mports.Clock) *Books {
	return &Books{
		Accounts: pgbilling.NewAccounts(pool), Customers: pgbilling.NewCustomers(pool), Catalog: pgbilling.NewCatalog(pool),
		Payments: pgbilling.NewPayments(pool), Subs: pgbilling.NewSubs(pool), Inbox: pgbilling.NewInbox(pool, clock, pgbilling.InboxOptions{}),
		Recharges: pgbilling.NewAutoRecharges(pool), Config: pgbilling.NewConfigWriter(pool),
	}
}

// Common is what both binaries give the payment layer.
type Common struct {
	Books    *Books
	Getenv   func(string) string // how an env: secret reference is read
	Clock    mports.Clock
	Metrics  mports.Metrics
	Bus      mports.Bus
	Log      *slog.Logger
	Settings envcfg.Payments

	SubjectPrefix string // PAYMENT_SUBJECT_PREFIX
}

// providers is the registry both stages verify with.
func (c Common) providers() (ports.Providers, error) {
	return Providers(ProviderDeps{Secrets: NewSecrets(c.Getenv), Catalog: c.Books.Catalog, Clock: c.Clock, Tolerance: c.Settings.WebhookTolerance})
}

func (c Common) ingress(providers ports.Providers) (*app.Ingress, error) {
	g, err := app.NewIngress(app.IngressDeps{
		Providers: providers, Accounts: c.Books.Accounts, Inbox: c.Books.Inbox, Metrics: c.Metrics, Clock: c.Clock, Bus: c.Bus, Log: c.Log,
		SubjectPrefix: c.SubjectPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("payments: ingress: %w", err)
	}
	return g, nil
}

// NewIngress is stage 1 of the webhook flow, for paymentd: it verifies, persists and acknowledges.
func NewIngress(c Common) (*app.Ingress, error) {
	providers, err := c.providers()
	if err != nil {
		return nil, err
	}
	return c.ingress(providers)
}

// Money is what only the worker uses: the way credits are minted and read back.
type Money struct {
	Meter    mports.Meter
	Lots     mports.LotReader
	Balances mports.BalanceReader
}

// NewWorker is the payment half of payment-worker: stage 2 of the webhook flow, the retry of parked deliveries, retention
// and auto top-up.
func NewWorker(c Common, m Money) (*Loops, error) {
	providers, err := c.providers()
	if err != nil {
		return nil, err
	}
	ingress, err := c.ingress(providers)
	if err != nil {
		return nil, err
	}
	processor, err := app.NewProcessor(app.ProcessorDeps{
		Inbox: c.Books.Inbox, Accounts: c.Books.Accounts, Providers: providers, Customers: c.Books.Customers, Catalog: c.Books.Catalog,
		Payments: c.Books.Payments, Subs: c.Books.Subs, Recharges: c.Books.Recharges, Meter: m.Meter, Lots: m.Lots,
		Clock: c.Clock, Metrics: c.Metrics, Bus: c.Bus, Log: c.Log, Grace: c.Settings.DunningGrace, SubjectPrefix: c.SubjectPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("payments: processor: %w", err)
	}
	recharge, err := app.NewAutoRecharge(app.AutoRechargeDeps{
		Instructions: c.Books.Recharges, Accounts: c.Books.Accounts, Providers: providers, Catalog: c.Books.Catalog, Customers: c.Books.Customers,
		Payments: c.Books.Payments, Balances: m.Balances, Clock: c.Clock, Metrics: c.Metrics, Bus: c.Bus, Log: c.Log, SubjectPrefix: c.SubjectPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("payments: auto top-up: %w", err)
	}
	return NewLoops(LoopsDeps{
		Processor: processor, Ingress: ingress, Inbox: c.Books.Inbox, Recharge: recharge, Bus: c.Bus, Clock: c.Clock, Log: c.Log,
		SubjectPrefix: c.SubjectPrefix, Tick: c.Settings.WebhookTick, Retention: c.Settings.EventRetention,
	})
}

// Sell is what paymentd serves besides the door a provider knocks on: selling, and managing what was sold. Neither has a
// Meter, so neither can grant a credit.
type Sell struct {
	Checkout *app.Checkout
	Changes  *app.PlanChanges
	Admin    *app.Admin
}

// NewSell builds the use cases behind the host's /v1 payment routes, over the same books and provider registry the webhook
// door verifies with, under the selling settings. A setting that cannot work stops it here.
func NewSell(c Common, s envcfg.Selling) (Sell, error) {
	providers, err := c.providers()
	if err != nil {
		return Sell{}, err
	}
	tax, err := app.NewTaxStrategy(s.TaxStrategy, nil)
	if err != nil {
		return Sell{}, fmt.Errorf("payments: %w", err)
	}
	checkout, err := app.NewCheckout(app.CheckoutDeps{
		Providers: providers, Accounts: c.Books.Accounts, Catalog: c.Books.Catalog, Customers: c.Books.Customers, Payments: c.Books.Payments,
		Tax: tax, Clock: c.Clock, ReturnHosts: s.ReturnHosts, Log: c.Log,
	})
	if err != nil {
		return Sell{}, fmt.Errorf("payments: checkout: %w", err)
	}
	changes, err := app.NewSubscriptions(app.SubscriptionsDeps{
		Providers: providers, Accounts: c.Books.Accounts, Catalog: c.Books.Catalog, Subs: c.Books.Subs, Payments: c.Books.Payments,
		Policy: s.PlanChangePolicy, Log: c.Log,
	})
	if err != nil {
		return Sell{}, fmt.Errorf("payments: subscriptions: %w", err)
	}
	names := make([]domain.Provider, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	admin, err := app.NewAdmin(app.AdminDeps{Writer: c.Books.Config, Providers: names, SecretSchemes: SecretSchemes})
	if err != nil {
		return Sell{}, fmt.Errorf("payments: admin: %w", err)
	}
	return Sell{Checkout: checkout, Changes: changes, Admin: admin}, nil
}

// NewDoor is the host's /v1 payment routes over the books and the use cases of Sell.
func NewDoor(c Common, sell Sell) (*paymenthttp.Door, error) {
	d, err := paymenthttp.New(paymenthttp.Config{
		Catalog: c.Books.Catalog, Checkout: sell.Checkout, Subs: c.Books.Subs, Changes: sell.Changes, Recharges: c.Books.Recharges,
		Payments: c.Books.Payments, Admin: sell.Admin, Caller: CredentialCaller{}, Logger: c.Log,
	})
	if err != nil {
		return nil, fmt.Errorf("payments: %w", err)
	}
	return d, nil
}
