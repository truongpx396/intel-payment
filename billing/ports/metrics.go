package ports

import mports "github.com/truongpx396/intel-payment/metering/ports"

// The metric names of the fiat layer (metering-ports.md § Observability contract). They are part of the contract:
// a host writes alerts against them before it reads the code, and the exporter declares every one of them from
// the first scrape. Constitution XII — every terminal state is observable.
//
//nolint:gosec // G101 false positive: these are metric NAMES
const (
	MetricWebhookReceived     = "billing_webhook_received_total"      // counter (provider, result: new|duplicate|parked)
	MetricWebhookVerifyFailed = "billing_webhook_verify_failed_total" // counter (provider) — a SECURITY event
	MetricWebhookInboxAge     = "billing_webhook_inbox_age_seconds"   // gauge
	MetricGrantApplied        = "billing_grant_applied_total"         // counter (realm, reason)
	MetricWebhookProcessed    = "billing_webhook_processed_total"     // counter (provider, outcome: processed|ignored)
	MetricWebhookIgnored      = "billing_webhook_ignored_total"       // counter (provider, reason)
	MetricWebhookFailed       = "billing_webhook_failed_total"        // counter (provider)
	MetricWebhookDead         = "billing_webhook_dead_total"          // counter (provider) — pages
	MetricWebhookForeignRealm = "billing_webhook_foreign_realm_total" // counter (provider) — a SECURITY event
)

// MetricCatalog lists every billing metric. A test holds it equal to the table in metering-ports.md, so a metric
// cannot be added to one and not the other.
var MetricCatalog = []mports.MetricDef{
	{Name: MetricWebhookReceived, Kind: mports.KindCounter, Labels: []string{"provider", "result"}, Unit: "{delivery}",
		Help: "Verified webhook deliveries by outcome: new, a duplicate acknowledged again, or parked because verification was unavailable"},
	{Name: MetricWebhookVerifyFailed, Kind: mports.KindCounter, Labels: []string{"provider"}, Unit: "{delivery}",
		Help: "Webhook deliveries whose signature did not verify: a security event"},
	{Name: MetricWebhookInboxAge, Kind: mports.KindGauge, Unit: "s",
		Help: "Age of the oldest webhook not yet fulfilled: a payment someone made that has not been processed"},
	{Name: MetricGrantApplied, Kind: mports.KindCounter, Labels: []string{"realm", "reason"}, Unit: "{grant}",
		Help: "Credit grants the processor applied, by reason; reconciles against the provider's payout report"},
	{Name: MetricWebhookProcessed, Kind: mports.KindCounter, Labels: []string{"provider", "outcome"}, Unit: "{event}",
		Help: "Inbox events the processor finished, as processed or ignored"},
	{Name: MetricWebhookIgnored, Kind: mports.KindCounter, Labels: []string{"provider", "reason"}, Unit: "{event}",
		Help: "Inbox events ignored by design: an unsubscribed type, a customer or price this deployment does not know"},
	{Name: MetricWebhookFailed, Kind: mports.KindCounter, Labels: []string{"provider"}, Unit: "{attempt}",
		Help: "Failed attempts to process an inbox event; each is retried on a backoff"},
	{Name: MetricWebhookDead, Kind: mports.KindCounter, Labels: []string{"provider"}, Unit: "{event}",
		Help: "Inbox events that failed every attempt: a verified payment event no one has fulfilled"},
	{Name: MetricWebhookForeignRealm, Kind: mports.KindCounter, Labels: []string{"provider"}, Unit: "{event}",
		Help: "Verified events whose customer resolves to a realm their account does not serve: a security event"},
}
