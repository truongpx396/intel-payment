// Package grpcclient is a remote ports.Meter: the generated metering.v1 client wrapped so that business
// code depends on ports.Meter and cannot tell an in-process value from this one (FR-044). Moving from the
// embedded library to the service is then a change of wiring, not of code.
//
// The client adds nothing of its own. It does not retry (every retry is the caller's, under the
// caller's idempotency key), invent a deadline, a key or a time, or round a value the wire cannot say.
// What the caller's context says is what the wire says; what the server answers is the typed error, or the
// Admission, the engine would have returned.
//
// Credentials ride on the connection (per-RPC credentials, mTLS), not on the client.
package grpcclient

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/wire"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Client is a remote Meter.
type Client struct{ c meteringv1.MeteringClient }

// New wraps a connection to a metering.v1 server.
func New(conn grpc.ClientConnInterface) *Client {
	return &Client{c: meteringv1.NewMeteringClient(conn)}
}

var (
	_ ports.Meter         = (*Client)(nil) // the swap is invisible to business code
	_ ports.BalanceReader = (*Client)(nil)
)

// Admit gates before expensive work. A refusal comes back as (Admission{Allowed: false, …}, nil), as it does
// from the engine: it is a decision, and the server only carries it as a status.
func (c *Client) Admit(ctx context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, error) {
	pb, err := wire.AdmitRequestToProto(s, req)
	if err != nil {
		return domain.Admission{}, err
	}
	reply, err := c.c.Admit(ctx, pb)
	if err != nil {
		if adm, ok := wire.DecodeDenial(err); ok {
			return adm, nil
		}
		return domain.Admission{}, wire.Decode(err)
	}
	adm, err := wire.AdmissionFromProto(reply)
	if err != nil {
		return domain.Admission{}, unreadable(err)
	}
	return adm, nil
}

// Record prices and settles. A deferred Record succeeded: Receipt.Deferred says it has not been applied yet.
func (c *Client) Record(ctx context.Context, e domain.Event) (domain.Receipt, error) {
	pb, err := wire.EventToProto(e)
	if err != nil {
		return domain.Receipt{}, err
	}
	reply, err := c.c.Record(ctx, &meteringv1.RecordRequest{Event: pb})
	if err != nil {
		return domain.Receipt{}, wire.Decode(err)
	}
	return wire.ReceiptFromProto(reply), nil
}

// Grant mints or removes credits. PRIVILEGED: the connection's credential needs `grant`.
func (c *Client) Grant(ctx context.Context, g domain.Grant) (domain.Receipt, error) {
	pb, err := wire.GrantToProto(g)
	if err != nil {
		return domain.Receipt{}, err
	}
	reply, err := c.c.Grant(ctx, pb)
	if err != nil {
		return domain.Receipt{}, wire.Decode(err)
	}
	return wire.ReceiptFromProto(reply), nil
}

// Transfer moves credits pool → allocation. PRIVILEGED.
func (c *Client) Transfer(ctx context.Context, t domain.Transfer) (domain.TransferReceipt, error) {
	reply, err := c.c.Transfer(ctx, wire.TransferToProto(t))
	if err != nil {
		return domain.TransferReceipt{}, wire.Decode(err)
	}
	return wire.TransferReceiptFromProto(reply), nil
}

// Balance reads the hot balance per pool.
func (c *Client) Balance(ctx context.Context, s domain.Scope) (map[domain.Pool]domain.Credits, error) {
	reply, err := c.c.GetBalance(ctx, &meteringv1.BalanceRequest{Scope: wire.ScopeToProto(s)})
	if err != nil {
		return nil, wire.Decode(err)
	}
	out := make(map[domain.Pool]domain.Credits, len(reply.GetPools()))
	for p, v := range reply.GetPools() {
		out[domain.Pool(p)] = domain.Credits(v)
	}
	return out, nil
}

// unreadable is an answer from the server this client cannot represent (a window it does not know, from a
// newer service). It is neither the caller's invalid request nor something to guess at, so it carries no
// domain sentinel.
func unreadable(err error) error {
	return fmt.Errorf("metering: the service sent an answer this client cannot read: %v", err) //nolint:errorlint // deliberately not wrapped: the cause is a conversion error that wraps ErrInvalid, and it was not the caller's request
}
