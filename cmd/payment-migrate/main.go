// Command payment-migrate applies the embedded, forward-only migrations.
//
//	payment-migrate up         apply every pending migration (idempotent; safe to run concurrently),
//	                           then install the Redis hot-path functions when a deploy URL is set
//	payment-migrate status     print the applied/pending versions
//	payment-migrate check      exit non-zero unless the schema is at head (what /readyz asks)
//	payment-migrate functions  install the Redis hot-path functions (FUNCTION LOAD REPLACE)
//
// The database is PAYMENT_LEDGER_DSN. The functions are installed with PAYMENT_BALANCE_REDIS_DEPLOY_URL,
// the deploy role's credential: only it may FUNCTION LOAD (deploy/redis/README.md), so the
// request-serving tier can never install code into the money store. There is deliberately no
// "down" (migrations/README.md).
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

	hotredis "github.com/truongpx396/intel-payment/metering/adapters/driven/redis"
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
		fmt.Fprintf(stderr, "usage: payment-migrate up|status|check|functions   (payment-migrate %s)\n", version)
		return 2
	}
	cmd := args[0]
	if cmd != "up" && cmd != "status" && cmd != "check" && cmd != "functions" {
		fmt.Fprintf(stderr, "unknown command %q; want up|status|check|functions\n", cmd)
		return 2
	}
	if cmd == "functions" {
		return installFunctions(ctx, getenv, stdout, stderr, true)
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
		if getenv("PAYMENT_BALANCE_REDIS_DEPLOY_URL") != "" {
			return installFunctions(ctx, getenv, stdout, stderr, false)
		}
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

// installFunctions loads the hot-path library with the deploy role's credential. required is true
// for the explicit `functions` command, which is an error without a URL; `up` installs only when a
// deploy URL is configured, so a deployment that loads functions some other way is not affected.
func installFunctions(ctx context.Context, getenv func(string) string, stdout, stderr io.Writer, required bool) int {
	url := getenv("PAYMENT_BALANCE_REDIS_DEPLOY_URL")
	if url == "" {
		if !required {
			return 0
		}
		fmt.Fprintln(stderr, "PAYMENT_BALANCE_REDIS_DEPLOY_URL is required: only the deploy role may FUNCTION LOAD")
		return 2
	}
	rdb, err := hotredis.Connect(url)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	defer func() { _ = rdb.Close() }()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	changed, err := hotredis.EnsureInstalled(ctx, rdb)
	if err != nil {
		fmt.Fprintln(stderr, "install hot-path functions:", err)
		return 1
	}
	if changed {
		fmt.Fprintf(stdout, "hot-path functions installed (library %s)\n", hotredis.LibraryName)
	} else {
		fmt.Fprintf(stdout, "hot-path functions already current (library %s)\n", hotredis.LibraryName)
	}
	return 0
}
