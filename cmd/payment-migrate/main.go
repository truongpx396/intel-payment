// Command payment-migrate applies the embedded, forward-only migrations.
//
//	payment-migrate up      apply every pending migration (idempotent; safe to run concurrently)
//	payment-migrate status  print the applied/pending versions
//	payment-migrate check   exit non-zero unless the schema is at head (what /readyz asks)
//
// The database is PAYMENT_LEDGER_DSN. There is deliberately no "down" (migrations/README.md).
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/truongpx396/intel-payment/migrate"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run returns the process exit code: 0 ok, 1 the operation failed or the schema is behind,
// 2 usage or configuration.
func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: payment-migrate up|status|check   (payment-migrate %s)\n", version)
		return 2
	}
	cmd := args[0]
	if cmd != "up" && cmd != "status" && cmd != "check" {
		fmt.Fprintf(stderr, "unknown command %q; want up|status|check\n", cmd)
		return 2
	}
	dsn := getenv("PAYMENT_LEDGER_DSN")
	if dsn == "" {
		fmt.Fprintln(stderr, "PAYMENT_LEDGER_DSN is required")
		return 2
	}
	ms, err := migrate.Embedded()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintln(stderr, "open:", err)
		return 2
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	switch cmd {
	case "up":
		done, err := migrate.Up(ctx, db, ms)
		for _, m := range done {
			fmt.Fprintf(stdout, "applied %04d_%s\n", m.Version, m.Name)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "schema at head (%04d), %d applied this run\n", len(ms), len(done))
	case "status", "check":
		st, err := migrate.Check(ctx, db, ms)
		fmt.Fprintf(stdout, "head=%04d current=%04d pending=%v ahead=%d\n", st.Head, st.Current, st.Pending, st.Ahead)
		if err != nil {
			fmt.Fprintln(stderr, err)
			if cmd == "check" || !errors.Is(err, migrate.ErrBehind) {
				return 1
			}
		}
	}
	return 0
}
