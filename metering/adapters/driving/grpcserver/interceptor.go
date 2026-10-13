package grpcserver

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/wire"
)

var errAuthBackend = errors.New("the credential could not be checked")

// AuthInterceptor authenticates every call. It lifts a Credential off the call — the bearer in the
// authorization metadata, the client certificates of the TLS handshake the listener already verified —
// and hands the handler the Principal it yields. A call without one never reaches a handler:
// UNAUTHENTICATED for a credential it does not recognise, INTERNAL (and logged) for one it could not check,
// which is the service failing and never a stranger's access.
func AuthInterceptor(a auth.Authenticator, log *slog.Logger) grpc.UnaryServerInterceptor {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		var cred auth.Credential
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			for _, v := range md.Get("authorization") {
				if scheme, token, found := strings.Cut(v, " "); found && strings.EqualFold(scheme, "Bearer") {
					cred.Bearer = strings.TrimSpace(token)
					break
				}
			}
		}
		if p, ok := peer.FromContext(ctx); ok {
			if info, ok := p.AuthInfo.(credentials.TLSInfo); ok {
				cred.PeerCertificates = info.State.PeerCertificates
			}
		}
		principal, err := a.Authenticate(ctx, cred)
		switch {
		case errors.Is(err, auth.ErrUnauthenticated):
			return nil, wire.Status(auth.ErrUnauthenticated).Err()
		case err != nil:
			log.Error("authentication failed", "error", err)
			return nil, wire.Status(errAuthBackend).Err()
		}
		return next(auth.WithPrincipal(ctx, principal), req)
	}
}
