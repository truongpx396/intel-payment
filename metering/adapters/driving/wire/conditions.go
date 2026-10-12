// Package wire is what the transports share: the table that fixes how each condition is reported
// (a gRPC code and reason, an HTTP status and code), the gRPC status encoding of it and its inverse, and
// the conversions between the wire messages and the domain.
//
// The table is the whole of the "status mapping is fixed" rule of grpc-surface.md and the error table of
// rest-api.md. A transport that needs a status looks it up here and invents none, and a test enumerates
// the exported errors of the domain and fails on any without a row, so a new error cannot ship unmapped.
package wire

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// ErrorDomain is the ErrorInfo.domain of every status this service writes. A status that carries another
// domain — a proxy's, a newer service's — is not decoded as ours.
const ErrorDomain = "metering.v1"

// Condition is one way a call ends other than success.
type Condition struct {
	// Reason is ErrorInfo.reason on the wire, unique per row. It is how a client recovers the typed error.
	Reason string
	// Err is the sentinel a caller tests with errors.Is. nil for a decision (a refused Admit is an
	// answer, not an error) and for the fallback.
	Err error
	// GRPC is the status code.
	GRPC codes.Code
	// HTTP and HTTPCode are the REST status and error.code. Several rows share one code where the family
	// is what a REST caller acts on (every hot-tier outage is 503 hot_store_unavailable).
	HTTP     int
	HTTPCode string
	// Verbatim: the caller may read the whole error text. True where the caller can act on the detail —
	// which field, which window, how stale. False for an unavailable or internal condition, whose chain
	// names the topology (a dial address, a DSN) and belongs in the operator's log, not in a response.
	Verbatim bool
}

func row(reason string, err error, g codes.Code, http int, code string, verbatim bool) Condition {
	return Condition{Reason: reason, Err: err, GRPC: g, HTTP: http, HTTPCode: code, Verbatim: verbatim}
}

// Table is ordered: Classify returns the first row an error matches, so a row that names a wrapper
// (ErrHotStoreUnavailable, which the engine wraps around a specific cause) precedes the rows it wraps.
var Table = []Condition{
	// Refused before the engine is reached.
	row("unauthenticated", auth.ErrUnauthenticated, codes.Unauthenticated, 401, "unauthenticated", true),
	row("forbidden", auth.ErrForbidden, codes.PermissionDenied, 403, "forbidden", true),
	row("realm_mismatch", auth.ErrRealmMismatch, codes.PermissionDenied, 403, "realm_mismatch", true),

	// The caller sent something the engine will never accept as it is.
	row("invalid_request", domain.ErrInvalid, codes.InvalidArgument, 400, "invalid_request", true),
	row("missing_subject", domain.ErrMissingSubject, codes.InvalidArgument, 400, "missing_subject", true),
	row("uncountable_limit", domain.ErrUncountableLimit, codes.InvalidArgument, 400, "uncountable_limit", true),
	row("amount_out_of_range", domain.ErrAmountOutOfRange, codes.InvalidArgument, 422, "amount_out_of_range", true),

	// Valid, but not in the state this server is in: a retry will not help until that state changes.
	row("unpriceable", domain.ErrUnpriceable, codes.FailedPrecondition, 422, "unpriceable", true),
	row("stale_event", domain.ErrStaleEvent, codes.FailedPrecondition, 422, "stale_event", true),
	row("dest_balance_cap", domain.ErrDestBalanceCap, codes.FailedPrecondition, 422, "dest_balance_cap", true),
	row("insufficient_balance", domain.ErrInsufficient, codes.FailedPrecondition, 409, "insufficient_balance", true),
	row("idempotency_conflict", domain.ErrIdemConflict, codes.AlreadyExists, 409, "idempotency_conflict", true),
	row("cross_realm", domain.ErrCrossRealm, codes.PermissionDenied, 403, "cross_realm", true),

	// The service cannot serve right now. ErrHotStoreUnavailable first: the engine wraps it around the
	// specific tier refusal below. Every one of them is 503 hot_store_unavailable to a REST caller.
	row("hot_store_unavailable", domain.ErrHotStoreUnavailable, codes.Unavailable, 503, "hot_store_unavailable", false),
	row("hot_shard_cold", domain.ErrColdShard, codes.Unavailable, 503, "hot_store_unavailable", false),
	row("hot_shard_frozen", domain.ErrShardFrozen, codes.Unavailable, 503, "hot_store_unavailable", false),
	row("outbox_backpressure", domain.ErrBackpressure, codes.Unavailable, 503, "hot_store_unavailable", false),
	row("hot_account_cold", domain.ErrColdScope, codes.Unavailable, 503, "hot_store_unavailable", false),
	row("quota_unavailable", domain.ErrQuotaUnavailable, codes.Unavailable, 503, "quota_unavailable", false),

	// The service is wrong, not the call. Loud.
	row("shard_count_mismatch", domain.ErrShardCountMismatch, codes.Internal, 500, "internal", false),
	row("quota_not_granted", domain.ErrQuotaNotGranted, codes.Internal, 500, "internal", false),

	// The caller's own context. Not a domain error, but a cancelled or timed-out call has an outcome.
	row("cancelled", context.Canceled, codes.Canceled, 499, "cancelled", false),
	row("deadline_exceeded", context.DeadlineExceeded, codes.DeadlineExceeded, 504, "deadline_exceeded", false),

	// A refused Admit is an answer. These rows have no sentinel: the Admission is what travels.
	row(domain.DenyPaymentRequired, nil, codes.ResourceExhausted, 402, "payment_required", true),
	row(domain.DenyLimitReached, nil, codes.ResourceExhausted, 409, "limit_reached", true),
	row(domain.DenyAccountBlocked, nil, codes.PermissionDenied, 403, "account_blocked", true),

	// Anything else. The message is fixed text; the error is in the operator's log.
	row("internal", nil, codes.Internal, 500, "internal", false),
}

// Internal is the fallback row.
var Internal = mustReason("internal")

func mustReason(reason string) Condition {
	c, ok := ByReason(reason)
	if !ok {
		panic("wire: no row " + reason)
	}
	return c
}

// ByReason returns the row with this reason.
func ByReason(reason string) (Condition, bool) {
	for _, c := range Table {
		if c.Reason == reason {
			return c, true
		}
	}
	return Condition{}, false
}

// Classify returns the row for err, seeing through any wrapping. ok is false for an error no row names;
// the caller reports Internal.
func Classify(err error) (Condition, bool) {
	if err == nil {
		return Condition{}, false
	}
	for _, c := range Table {
		if c.Err != nil && errors.Is(err, c.Err) {
			return c, true
		}
	}
	return Condition{}, false
}

// Denial returns the row for a refused Admission: by the deny code of the ceiling that refused it, or
// account_blocked for a held account. ok is false for a refusal that names neither — a refusal the engine
// did not classify, which the transport reports as Internal rather than guess.
func Denial(adm domain.Admission) (Condition, bool) {
	if adm.Allowed {
		return Condition{}, false
	}
	if adm.Blocked {
		return mustReason(domain.DenyAccountBlocked), true
	}
	if adm.Exceeded == nil {
		return Condition{}, false
	}
	switch adm.Exceeded.DenyCode {
	case domain.DenyPaymentRequired, domain.DenyLimitReached, domain.DenyAccountBlocked:
		return mustReason(adm.Exceeded.DenyCode), true
	}
	return Condition{}, false
}
