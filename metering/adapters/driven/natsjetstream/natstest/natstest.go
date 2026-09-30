// Package natstest starts a real NATS server with JetStream for tests, so the JetStream bus is held
// to the same contract as the Redis one against the real thing rather than a fake.
package natstest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the NATS server under test.
const Image = "nats:2.11-alpine"

// Server is a running NATS server.
type Server struct {
	URL       string
	Container testcontainers.Container
}

// Run starts a single-node server with JetStream and a file store. (Production runs R3 or R5; a
// single node exercises every behaviour of the adapter but the replication itself.)
func Run(ctx context.Context) (*Server, error) {
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        Image,
			ExposedPorts: []string{"4222/tcp"},
			Cmd:          []string{"-js", "-sd", "/data"},
			WaitingFor:   wait.ForLog("Server is ready").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return nil, fmt.Errorf("natstest: start nats: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		return nil, err
	}
	port, err := c.MappedPort(ctx, "4222/tcp")
	if err != nil {
		return nil, err
	}
	return &Server{URL: fmt.Sprintf("nats://%s:%s", host, port.Port()), Container: c}, nil
}

// Start runs a server for one test and stops it with the test.
func Start(ctx context.Context, tb testing.TB) *Server {
	tb.Helper()
	s, err := Run(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Terminate(context.Background()) })
	return s
}

// Terminate stops the container.
func (s *Server) Terminate(_ context.Context) error {
	return testcontainers.TerminateContainer(s.Container)
}

// Connect opens a connection. Each caller owns and closes its own.
func (s *Server) Connect(opts ...nats.Option) (*nats.Conn, error) {
	return nats.Connect(s.URL, append([]nats.Option{nats.Timeout(5 * time.Second)}, opts...)...)
}
