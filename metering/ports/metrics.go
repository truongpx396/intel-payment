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
)
