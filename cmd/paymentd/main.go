// Command paymentd serves the metering engine over gRPC (the hot path) and REST, behind a service credential.
//
//	paymentd              serve (the default)
//	paymentd serve        the same
//	paymentd healthcheck  exit 0 only while this host's paymentd answers /readyz with 200; for a container
//	                      healthcheck, because the image is distroless and has no curl
//
// Everything it is told arrives as PAYMENT_* variables (docs/configuration.md), read here and nowhere else,
// and checked before anything is dialled. It is the request-serving tier: the durable writer is
// payment-worker, and paymentd never books the ledger itself.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run returns the process exit code: 0 a clean stop (or a healthy check), 1 it ran and failed or refused to
// start against what it found, 2 usage or configuration.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 1 {
		cmd = "?"
	} else if len(args) == 1 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serve(ctx, getenv, stdout, stderr)
	case "healthcheck":
		return healthcheck(ctx, getenv, stderr)
	default:
		fmt.Fprintf(stderr, "usage: paymentd [serve|healthcheck]   (paymentd %s)\n", version)
		return 2
	}
}
