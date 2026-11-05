package events

import "github.com/truongpx396/intel-payment/metering/ports"

// The metrics of outbound delivery. Like the engine's, the names are part of the contract: a host writes alerts against them.
const (
	MetricRecorded       = "events_recorded_total"        // counter (type)
	MetricDelivery       = "events_delivery_total"        // counter (outcome: delivered|retry|dead)
	MetricDeliveryFailed = "events_delivery_failed_total" // counter (realm, endpoint)
	MetricDeadDeliveries = "events_dead_deliveries"       // gauge (realm)
	MetricDeliveryLag    = "events_delivery_lag_seconds"  // histogram: from the event to its first 2xx
)

// MetricCatalog lists every metric of outbound delivery. An exporter registers it beside the engine's, and a test holds it equal to the
// table in metering-ports.md.
var MetricCatalog = []ports.MetricDef{
	{Name: MetricRecorded, Kind: ports.KindCounter, Labels: []string{"type"}, Unit: "{event}", Help: "Events appended to the outbound log"},
	{Name: MetricDelivery, Kind: ports.KindCounter, Labels: []string{"outcome"}, Unit: "{attempt}", Help: "Delivery attempts by outcome: delivered, retry, dead"},
	{Name: MetricDeliveryFailed, Kind: ports.KindCounter, Labels: []string{"realm", "endpoint"}, Unit: "{delivery}", Help: "Deliveries marked dead after the retry window: each is one page"},
	{Name: MetricDeadDeliveries, Kind: ports.KindGauge, Labels: []string{"realm"}, Unit: "{delivery}", Help: "Dead deliveries nobody has re-driven"},
	{Name: MetricDeliveryLag, Kind: ports.KindHistogram, Labels: []string{"realm"}, Unit: "s", Help: "Seconds from an event being recorded to its delivery"},
}
