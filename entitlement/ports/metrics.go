package ports

import mports "github.com/truongpx396/intel-payment/metering/ports"

// The metric names of the entitlement layer (entitlement-ports.md § Observability). They are part of the contract: a host
// writes alerts against them before it reads the code. Constitution XII — every terminal state is observable.
const (
	MetricDecisions = "entitlement_decisions_total" // counter (realm, outcome: allowed|denied|error)
	MetricInvalid   = "entitlement_invalid_total"   // counter (realm, source) — a stored value its key cannot hold

	MetricCacheLookups       = "entitlement_cache_total"               // counter (result: hit|miss)
	MetricCacheInvalidations = "entitlement_cache_invalidations_total" // counter (source: scope|realm)
)

// MetricCatalog lists every entitlement metric. A test holds it equal to the table in entitlement-ports.md, so a metric
// cannot be added to one and not the other.
var MetricCatalog = []mports.MetricDef{
	{Name: MetricDecisions, Kind: mports.KindCounter, Labels: []string{"realm", "outcome"}, Unit: "{decision}",
		Help: "Entitlement checks by outcome: allowed, denied, or error (the store could not answer and the host denied)"},
	{Name: MetricInvalid, Kind: mports.KindCounter, Labels: []string{"realm", "source"}, Unit: "{grant}",
		Help: "Stored grants skipped because their key cannot hold them: an undeclared enum member or the wrong kind. Never granted; a data bug"},
	{Name: MetricCacheLookups, Kind: mports.KindCounter, Labels: []string{"result"}, Unit: "{lookup}",
		Help: "Resolutions served from the cache (hit) or read from the store (miss)"},
	{Name: MetricCacheInvalidations, Kind: mports.KindCounter, Labels: []string{"source"}, Unit: "{announcement}",
		Help: "Announcements applied to the cache: one scope, or a whole realm. Zero over a day with subscription changes means the replica's Watch is not receiving"},
}
