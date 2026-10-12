//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pgadapter "github.com/truongpx396/intel-payment/metering/adapters/driven/postgres"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// GET /v1/ledger and the credits panel read the account of record. A page is a keyset over (created_at, id),
// newest first: every row exactly once across pages however many are booked between them, and a member's
// view holds only the rows that member's spend produced.

func bookRows(t *testing.T, b *pgadapter.Books, s domain.Scope, rows ...ports.LedgerRow) {
	t.Helper()
	seqNo := seq.Add(1) * 1000
	for i := range rows {
		rows[i].Scope = s
		rows[i].Gen, rows[i].SeqFrom, rows[i].SeqTo = 1, seqNo+int64(i), seqNo+int64(i)
		if rows[i].IdemKey == "" {
			rows[i].IdemKey = fmt.Sprintf("k-%d-%d", seqNo, i)
		}
	}
	must(t, b.InTx(context.Background(), s, func(ctx context.Context, tx ports.BookTx) error {
		_, err := tx.Book(ctx, rows)
		return err
	}))
}

func walk(t *testing.T, b *pgadapter.Books, q ports.LedgerQuery) (all []ports.LedgerEntry, pages int) {
	t.Helper()
	for {
		page, err := b.LedgerRows(context.Background(), q)
		must(t, err)
		pages++
		all = append(all, page.Rows...)
		if page.Next == "" {
			return all, pages
		}
		q.After = page.Next
		if pages > 100 {
			t.Fatal("the cursor never ends")
		}
	}
}

func TestLedgerRowsPageEveryRowExactlyOnceNewestFirst(t *testing.T) {
	t.Parallel()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	other := domain.Scope{Realm: "t1", Kind: "workspace", ID: "someone-else"}
	for i := 0; i < 120; i++ {
		bookRows(t, b, s, ports.LedgerRow{Delta: domain.Credits(i + 1), OperationType: "purchase"})
	}
	bookRows(t, b, other, ports.LedgerRow{Delta: 999, OperationType: "purchase"})

	got, pages := walk(t, b, ports.LedgerQuery{Scope: s, Limit: 50})
	if len(got) != 120 || pages != 3 {
		t.Fatalf("120 rows in pages of 50: got %d rows in %d pages", len(got), pages)
	}
	seen := map[string]bool{}
	for i, e := range got {
		if seen[e.ID] {
			t.Fatalf("row %s appears twice", e.ID)
		}
		seen[e.ID] = true
		if e.Scope != s {
			t.Fatalf("another scope's row: %+v", e)
		}
		if want := domain.Credits(120 - i); e.Delta != want {
			t.Fatalf("row %d: delta %d, want %d: newest first", i, e.Delta, want)
		}
		if i > 0 && e.CreatedAt.After(got[i-1].CreatedAt) {
			t.Fatalf("row %d is newer than the one before it", i)
		}
	}
}

func TestAPageIsStableWhileRowsAreBooked(t *testing.T) {
	t.Parallel()
	b := pgadapter.NewBooks(db(t))
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	for i := 0; i < 30; i++ {
		bookRows(t, b, s, ports.LedgerRow{Delta: domain.Credits(i + 1), OperationType: "purchase"})
	}
	first, err := b.LedgerRows(context.Background(), ports.LedgerQuery{Scope: s, Limit: 10})
	must(t, err)
	// Spend lands between two page reads.
	for i := 0; i < 5; i++ {
		bookRows(t, b, s, ports.LedgerRow{Delta: -1, OperationType: "query"})
	}
	rest, _ := walk(t, b, ports.LedgerQuery{Scope: s, Limit: 10, After: first.Next})
	if len(first.Rows)+len(rest) != 30 {
		t.Fatalf("rows booked after the first page must not shift the next ones: %d + %d", len(first.Rows), len(rest))
	}
	for _, e := range rest {
		if e.Delta < 0 {
			t.Fatal("a row booked after the cursor was returned behind it")
		}
	}
}

func TestPagingSurvivesRowsWithTheSameTimestamp(t *testing.T) {
	t.Parallel()
	p := db(t)
	b := pgadapter.NewBooks(p)
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	bookRows(t, b, s, ports.LedgerRow{Delta: 1, OperationType: "x"}, ports.LedgerRow{Delta: 2, OperationType: "x"}, ports.LedgerRow{Delta: 3, OperationType: "x"},
		ports.LedgerRow{Delta: 4, OperationType: "x"}, ports.LedgerRow{Delta: 5, OperationType: "x"}, ports.LedgerRow{Delta: 6, OperationType: "x"})
	if _, err := p.Exec(context.Background(), `UPDATE credit_ledger SET created_at = '2026-09-29T12:00:00Z' WHERE scope_id = $1`, s.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := walk(t, b, ports.LedgerQuery{Scope: s, Limit: 2})
	if len(got) != 6 {
		t.Fatalf("a timestamp is not a key: %d of 6 rows", len(got))
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	if len(ids) != 6 {
		t.Fatalf("duplicates across pages: %v", ids)
	}
}

func TestEveryFieldOfARowIsReturned(t *testing.T) {
	t.Parallel()
	b := pgadapter.NewBooks(db(t))
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	cp := domain.Scope{Realm: "t1", Kind: "organization", ID: "org-1"}
	occurred := time.Date(2026, 9, 29, 12, 0, 0, 123_000_000, time.UTC)
	bookRows(t, b, s,
		ports.LedgerRow{Pool: "promo", Delta: -660, OperationType: "query", IdemKey: "evt-1", EventCount: 1, RateCardVersion: "v1", CostMicros: 5, Resource: "llm.chat",
			RateKey: "gpt", Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}, {Unit: "out", Amount: 800}}, Subjects: domain.Subjects{"user": "u1"},
			Ref: map[string]string{"trace_id": "t-1"}, OccurredAt: occurred},
		ports.LedgerRow{Delta: -2000, OperationType: "allocation_out", IdemKey: "alloc-1", Counterparty: &cp},
		ports.LedgerRow{Delta: 40, OperationType: "writeoff", IdemKey: "wo-1"},
	)
	got, _ := walk(t, b, ports.LedgerQuery{Scope: s, Limit: 10})
	if len(got) != 3 {
		t.Fatalf("%d rows", len(got))
	}
	by := map[string]ports.LedgerEntry{}
	for _, e := range got {
		by[e.IdemKey] = e
	}
	q := by["evt-1"]
	if q.ID == "" || q.CreatedAt.IsZero() || q.Pool != "promo" || q.Delta != -660 || q.OperationType != "query" || q.EventCount != 1 || q.RateCardVersion != "v1" ||
		q.Resource != "llm.chat" || q.RateKey != "gpt" || len(q.Quantities) != 2 || q.Quantities[1].Amount != 800 || q.Subjects["user"] != "u1" ||
		q.Ref["trace_id"] != "t-1" || !q.OccurredAt.Equal(occurred) || q.SeqTo < 1 || q.Counterparty != nil {
		t.Fatalf("%+v", q)
	}
	a := by["alloc-1"]
	if a.Counterparty == nil || *a.Counterparty != cp || a.Delta != -2000 || a.Pool != domain.GeneralPool {
		t.Fatalf("a transfer row names its counterparty: %+v", a)
	}
	if w := by["wo-1"]; w.Delta != 40 || w.RateCardVersion != "" || len(w.Quantities) != 0 || w.Subjects != nil {
		t.Fatalf("a non-priced row carries no card or quantities: %+v", w)
	}
}

func TestAMembersViewHoldsOnlyThatMembersRows(t *testing.T) {
	t.Parallel()
	b := pgadapter.NewBooks(db(t))
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: fmt.Sprintf("w-%d", seq.Add(1))}
	bookRows(t, b, s,
		ports.LedgerRow{Delta: -10, OperationType: "query", Subjects: domain.Subjects{"user": "u1"}},
		ports.LedgerRow{Delta: -20, OperationType: "query", Subjects: domain.Subjects{"user": "u2"}},
		ports.LedgerRow{Delta: -30, OperationType: "query", Subjects: domain.Subjects{"user": "u1", "api_key": "k7"}},
		ports.LedgerRow{Delta: -40, OperationType: "query", Subjects: domain.Subjects{"user": "u10"}},
		ports.LedgerRow{Delta: -50, OperationType: "query", Subjects: domain.Subjects{"api_key": "u1"}},
		ports.LedgerRow{Delta: 1000, OperationType: "purchase"},
	)
	got, _ := walk(t, b, ports.LedgerQuery{Scope: s, SubjectKind: "user", SubjectID: "u1", Limit: 10})
	var sum domain.Credits
	for _, e := range got {
		if e.Subjects["user"] != "u1" {
			t.Fatalf("another member's row (or an unattributed one): %+v", e)
		}
		sum += e.Delta
	}
	if len(got) != 2 || sum != -40 {
		t.Fatalf("u1 spent 10 + 30: %d rows, %d (u10 is not u1, and a key named u1 is not the user u1)", len(got), sum)
	}
	if all, _ := walk(t, b, ports.LedgerQuery{Scope: s, Limit: 10}); len(all) != 6 {
		t.Fatalf("the unscoped view holds everything: %d", len(all))
	}
}

func TestARealmIsolatesLedgerReads(t *testing.T) {
	t.Parallel()
	b := pgadapter.NewBooks(db(t))
	id := fmt.Sprintf("w-%d", seq.Add(1))
	mine, theirs := domain.Scope{Realm: "t1", Kind: "workspace", ID: id}, domain.Scope{Realm: "t2", Kind: "workspace", ID: id}
	bookRows(t, b, mine, ports.LedgerRow{Delta: 1, OperationType: "purchase"})
	bookRows(t, b, theirs, ports.LedgerRow{Delta: 2, OperationType: "purchase"}, ports.LedgerRow{Delta: 3, OperationType: "purchase"})
	got, _ := walk(t, b, ports.LedgerQuery{Scope: mine, Limit: 10})
	if len(got) != 1 || got[0].Delta != 1 {
		t.Fatalf("one product's ledger is not another's: %+v", got)
	}
}

func TestABadCursorOrLimitIsInvalid(t *testing.T) {
	t.Parallel()
	b := pgadapter.NewBooks(db(t))
	s := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w"}
	for name, q := range map[string]ports.LedgerQuery{
		"a cursor that is not ours":   {Scope: s, After: "not-a-cursor"},
		"a cursor of the wrong shape": {Scope: s, After: "MjAyNi0wOS0yOXxub3QtYS11dWlk"},
		"a limit above the maximum":   {Scope: s, Limit: 201},
		"a negative limit":            {Scope: s, Limit: -1},
		"a bad scope":                 {Scope: domain.Scope{Realm: "t1", Kind: "Bad Kind", ID: "x"}},
		"half a member":               {Scope: s, SubjectKind: "user"},
		"the other half":              {Scope: s, SubjectID: "u1"},
	} {
		if _, err := b.LedgerRows(context.Background(), q); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if page, err := b.LedgerRows(context.Background(), ports.LedgerQuery{Scope: s}); err != nil || len(page.Rows) != 0 || page.Next != "" {
		t.Fatalf("no limit means the default, and an empty ledger is an empty page: %+v %v", page, err)
	}
}
