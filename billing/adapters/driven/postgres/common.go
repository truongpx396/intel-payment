package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/truongpx396/intel-payment/billing/domain"
	md "github.com/truongpx396/intel-payment/metering/domain"
)

// Postgres error classes the stores translate into the domain's errors.
const (
	codeUniqueViolation = "23505"
	codeCheckViolation  = "23514"
	codeForeignKey      = "23503"
	codeBadText         = "22P02" // invalid text representation: a malformed uuid
)

// mapErr translates a database error into the domain's, keeping the cause for the operator's log.
func mapErr(err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case codeUniqueViolation:
		return fmt.Errorf("%w: %s (%s)", md.ErrAlreadyExists, pe.Message, pe.ConstraintName)
	case codeCheckViolation:
		return fmt.Errorf("%w: %s (%s)", md.ErrInvalid, pe.Message, pe.ConstraintName)
	case codeBadText:
		return fmt.Errorf("%w: %s", md.ErrNotFound, pe.Message)
	}
	return err
}

// db is what a pool and a transaction have in common.
type db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// inTx runs fn in a transaction, committing if it returns nil.
func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return mapErr(err)
	}
	return nil
}

// lockKey takes a transaction-scoped advisory lock named by a string. Rows that do not exist yet cannot be locked
// with SELECT … FOR UPDATE, so the lock is on the name: two read-modify-writes of one payment, one subscription —
// including the one that creates it — run one after the other.
func lockKey(ctx context.Context, tx pgx.Tx, name string) error {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(h.Sum64())); err != nil { //nolint:gosec // a hash used as a lock id: wrapping is fine
		return fmt.Errorf("postgres: lock %s: %w", name, err)
	}
	return nil
}

// utc is a time as the database keeps it: UTC, to the microsecond.
func utc(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := utc(*t)
	return &v
}

// optTime is a time the schema allows to be NULL, for a value that is zero when unset.
func optTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := utc(t)
	return &v
}

func fromOpt(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func orEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func realmOf(s domain.Scope) string { return string(s.Realm.Or()) }

// cursor-free helper: a page of at most `limit` rows, 0 meaning the default and above the maximum clamped.
const (
	defaultPage = 50
	maxPage     = 200
)

func pageSize(limit int) (int, error) {
	switch {
	case limit < 0:
		return 0, fmt.Errorf("%w: limit %d", md.ErrInvalid, limit)
	case limit == 0:
		return defaultPage, nil
	case limit > maxPage:
		return maxPage, nil
	}
	return limit, nil
}

// errInvalid is the domain's ErrInvalid, named once for this package.
var errInvalid = md.ErrInvalid

var (
	errNotFound      = md.ErrNotFound
	errAlreadyExists = md.ErrAlreadyExists
)
