package ports

import (
	"context"
	"time"
)

type receivedAtKey struct{}

// WithReceivedAt returns a context that says when the delivery being verified ARRIVED. The ingress sets it when it
// verifies a parked delivery again, because a delivery whose verification was unavailable is answered 202 and the
// provider will not send it again: if its signature were judged by the time of the retry, an outage longer than the
// tolerance would make every genuine delivery in the parked half look like a replay, and a rejected parked delivery is
// dropped. A delivery verified when it arrives carries no such time, and arrives "now".
func WithReceivedAt(ctx context.Context, at time.Time) context.Context {
	return context.WithValue(ctx, receivedAtKey{}, at)
}

// ReceivedAt is the instant the delivery being verified arrived: the one WithReceivedAt put in ctx, or fallback — the
// adapter's own "now" — when it carries none. An adapter measures a signature's age against it.
func ReceivedAt(ctx context.Context, fallback time.Time) time.Time {
	if at, ok := ctx.Value(receivedAtKey{}).(time.Time); ok && !at.IsZero() {
		return at
	}
	return fallback
}
