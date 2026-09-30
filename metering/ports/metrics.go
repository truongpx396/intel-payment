package ports

// The metric names of the observability contract (metering-ports.md § Observability contract).
// They are part of the contract, not an implementation detail: a host writes alerts against them
// before it ever reads the code. Invariant 15 — every terminal state is observable.
//
//nolint:gosec // G101 false positive: these are metric NAMES, "credits" is a unit, not a credential
const (
	MetricAdmitLatency      = "metering_admit_latency_seconds"           // histogram (realm, outcome)
	MetricAdmitDenied       = "metering_admit_denied_total"              // counter (realm, limit, deny_code)
	MetricAdmitUncapped     = "metering_admit_uncapped_total"            // counter (realm)
	MetricAdmitFailOpen     = "metering_admit_failopen_total"            // counter (realm)
	MetricRecordDeferred    = "metering_record_deferred_total"           // counter (realm)
	MetricJournalPending    = "metering_journal_pending"                 // gauge (realm)
	MetricDriftFindings     = "metering_drift_findings_total"            // counter (realm, outcome)
	MetricDriftAbsCredits   = "metering_drift_abs_credits"               // gauge (shard)
	MetricReconcileDeferred = "metering_reconcile_deferred_total"        // counter (shard)
	MetricOutboxDepth       = "metering_outbox_depth"                    // gauge (shard)
	MetricOutboxAge         = "metering_outbox_age_seconds"              // gauge (shard)
	MetricWriterSeqGap      = "metering_writer_seq_gap_seconds"          // gauge (shard)
	MetricHotRegression     = "metering_hot_regression_total"            // counter (shard)
	MetricShardFrozen       = "metering_shard_frozen_seconds"            // gauge (shard)
	MetricLateReplay        = "metering_late_replay_total"               // counter (realm)
	MetricIdemConflict      = "metering_idem_conflict_total"             // counter (realm, op)
	MetricSuspenseOpen      = "metering_suspense_open"                   // gauge (realm, reason)
	MetricTransferAge       = "metering_transfer_in_transit_age_seconds" // gauge (realm)
	MetricRehydrate         = "metering_rehydrate_total"                 // counter (realm, cause)
	MetricPriceError        = "metering_price_error_total"               // counter (realm, rate_key, reason)
	MetricStaleEvent        = "metering_stale_event_total"               // counter (realm)
	MetricDefaultPartition  = "metering_default_partition_rows"          // gauge (table)

	// Terminal states the writer reaches that no other metric shows (invariant 15).
	MetricEffectFailed     = "metering_effect_failed_total"     // counter (kind)
	MetricCorrectionOrphan = "metering_correction_orphan_total" // counter (realm)
	MetricCorrectionStale  = "metering_correction_stale_total"  // counter (realm)
	MetricRecoverParked    = "metering_recover_parked_total"    // counter (shard)
)

// MetricKind is how a metric is aggregated.
type MetricKind string

const (
	KindCounter   MetricKind = "counter"
	KindGauge     MetricKind = "gauge"
	KindHistogram MetricKind = "histogram"
)

// MetricDef is one metric of the observability contract.
type MetricDef struct {
	Name   string
	Kind   MetricKind
	Labels []string
	Unit   string // UCUM-style: "s", "{credit}", "{entry}", "{row}", "1"
	Help   string
}

// Catalog lists every metering metric. It is what an exporter registers, and a test holds it equal to
// the table in metering-ports.md, so a metric cannot be added to one and not the other.
var Catalog = []MetricDef{
	{MetricAdmitLatency, KindHistogram, []string{"realm", "outcome"}, "s", "Admit latency: on every request's critical path"},
	{MetricAdmitDenied, KindCounter, []string{"realm", "limit", "deny_code"}, "{denial}", "Admissions denied"},
	{MetricAdmitUncapped, KindCounter, []string{"realm"}, "{call}", "Admits without MaxCost: the overshoot bound is not enforced"},
	{MetricAdmitFailOpen, KindCounter, []string{"realm"}, "{call}", "Work served while the hot tier was unavailable"},
	{MetricRecordDeferred, KindCounter, []string{"realm"}, "{event}", "Usage journaled during an outage"},
	{MetricJournalPending, KindGauge, []string{"realm"}, "{entry}", "Deferred usage not yet applied to the hot tier"},
	{MetricDriftFindings, KindCounter, []string{"realm", "outcome"}, "{finding}", "Drift or regression at equal sequence numbers"},
	{MetricDriftAbsCredits, KindGauge, []string{"shard"}, "{credit}", "Sum of |drift| in the last reconcile"},
	{MetricReconcileDeferred, KindCounter, []string{"shard"}, "{scope}", "Scopes with intents in flight at reconcile time"},
	{MetricOutboxDepth, KindGauge, []string{"shard"}, "{entry}", "Intents not yet booked"},
	{MetricOutboxAge, KindGauge, []string{"shard"}, "s", "Age of the oldest intent not yet booked"},
	{MetricWriterSeqGap, KindGauge, []string{"shard"}, "s", "Age of the oldest unfilled sequence gap"},
	{MetricHotRegression, KindCounter, []string{"shard"}, "{event}", "The hot tier lost history"},
	{MetricShardFrozen, KindGauge, []string{"shard"}, "s", "How long a shard was frozen"},
	{MetricLateReplay, KindCounter, []string{"realm"}, "{event}", "A replay after its hot guard expired"},
	{MetricIdemConflict, KindCounter, []string{"realm", "op"}, "{event}", "A key reused for a different request"},
	{MetricSuspenseOpen, KindGauge, []string{"realm", "reason"}, "{entry}", "Open suspense entries"},
	{MetricTransferAge, KindGauge, []string{"realm"}, "s", "Age of the oldest transfer in transit"},
	{MetricRehydrate, KindCounter, []string{"realm", "cause"}, "{account}", "Accounts rebuilt from the books"},
	{MetricPriceError, KindCounter, []string{"realm", "rate_key", "reason"}, "{event}", "Fail-closed pricing refusing traffic"},
	{MetricStaleEvent, KindCounter, []string{"realm"}, "{event}", "Events refused as older than the dedup window"},
	{MetricDefaultPartition, KindGauge, []string{"table"}, "{row}", "Rows in a DEFAULT partition"},
	{MetricEffectFailed, KindCounter, []string{"kind"}, "{effect}", "A follow-up to a booking failed; a sweep will finish it"},
	{MetricCorrectionOrphan, KindCounter, []string{"realm"}, "{event}", "A correction closed no suspense entry"},
	{MetricCorrectionStale, KindCounter, []string{"realm"}, "{entry}", "Open suspense past the hot guard's lifetime: a human's problem"},
	{MetricRecoverParked, KindCounter, []string{"shard"}, "{entry}", "Intents parked by a recovery: they sat behind a gap that cannot fill"},
}
