package grpcclient

import (
	"context"
	"errors"

	"google.golang.org/grpc"
)

// BearerOption configures WithBearer.
type BearerOption func(*bearer)

// OverInsecureTransport lets the token ride a connection that is not TLS. A bearer token is a password, so
// by default it is refused one; say so only for a transport whose network is the credential (loopback, a
// sidecar's Unix socket).
func OverInsecureTransport() BearerOption { return func(b *bearer) { b.requireTLS = false } }

type bearer struct {
	token      string
	requireTLS bool
}

func (b bearer) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	if b.token == "" {
		return nil, errors.New("grpcclient: the bearer token is empty: refusing to send an anonymous call as though it were authenticated")
	}
	return map[string]string{"authorization": "Bearer " + b.token}, nil
}

func (b bearer) RequireTransportSecurity() bool { return b.requireTLS }

// WithBearer puts the service credential on every call of the connection, so the code that holds a
// ports.Meter never handles a token.
func WithBearer(token string, opts ...BearerOption) grpc.DialOption {
	b := bearer{token: token, requireTLS: true}
	for _, o := range opts {
		o(&b)
	}
	return grpc.WithPerRPCCredentials(b)
}
