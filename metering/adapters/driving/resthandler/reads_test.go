package resthandler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// stubReads is a programmable LedgerReader and TransferReader.
type stubReads struct {
	mu      sync.Mutex
	rows    func(context.Context, ports.LedgerQuery) (ports.LedgerPage, error)
	xfer    func(context.Context, domain.Realm, string) (ports.TransferRecord, bool, error)
	gotQ    ports.LedgerQuery
	gotRlm  domain.Realm
	gotKey  string
	queries int
}

var (
	_ ports.LedgerReader   = (*stubReads)(nil)
	_ ports.TransferReader = (*stubReads)(nil)
)

func (r *stubReads) LedgerRows(ctx context.Context, q ports.LedgerQuery) (ports.LedgerPage, error) {
	r.mu.Lock()
	r.gotQ = q
	r.queries++
	r.mu.Unlock()
	if r.rows == nil {
		return ports.LedgerPage{}, nil
	}
	return r.rows(ctx, q)
}

func (r *stubReads) Transfer(ctx context.Context, realm domain.Realm, key string) (ports.TransferRecord, bool, error) {
	r.mu.Lock()
	r.gotRlm, r.gotKey = realm, key
	r.queries++
	r.mu.Unlock()
	if r.xfer == nil {
		return ports.TransferRecord{}, false, nil
	}
	return r.xfer(ctx, realm, key)
}

func ledgerRoute(query string) route {
	return route{"ledger", "GET", "/v1/ledger?scope_kind=workspace&scope_id=w1" + query, "", false}
}

func TestTheLedgerIsReadThroughTheCredentialsRealmAndPaged(t *testing.T) {
	t.Parallel()
	at := mustTime("2026-09-29T12:00:00.5Z")
	cp := domain.Scope{Realm: "acme", Kind: "organization", ID: "org-1"}
	reads := &stubReads{rows: func(_ context.Context, q ports.LedgerQuery) (ports.LedgerPage, error) {
		return ports.LedgerPage{Next: "cur-2", Rows: []ports.LedgerEntry{
			{ID: "id-1", CreatedAt: at, LedgerRow: ports.LedgerRow{
				Scope: q.Scope, Pool: "promo", Delta: -660, OperationType: "query", IdemKey: "evt-1", SeqFrom: 7, SeqTo: 7, EventCount: 1, RateCardVersion: "v1", Resource: "llm.chat",
				RateKey: "gpt", Quantities: []domain.Quantity{{Unit: "in", Amount: 1200}}, Subjects: domain.Subjects{"user": "u1"}, Ref: map[string]string{"trace_id": "t"}, OccurredAt: at,
			}},
			{ID: "id-2", CreatedAt: at, LedgerRow: ports.LedgerRow{Scope: q.Scope, Pool: "general", Delta: -2000, OperationType: "allocation_out", IdemKey: "alloc-1", SeqFrom: 8, SeqTo: 8, EventCount: 1, Counterparty: &cp}},
		}}, nil
	}}
	a := newAPI(t, &stubMeter{}, apiOpts{ledger: reads})
	rec := a.do("record", ledgerRoute("&limit=2&cursor=cur-1"))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if q := reads.gotQ; q.Scope != (domain.Scope{Realm: "acme", Kind: "workspace", ID: "w1"}) || q.Limit != 2 || q.After != "cur-1" || q.SubjectKind != "" {
		t.Fatalf("the reader was asked %+v", q)
	}
	var got struct {
		Data []map[string]any `json:"data"`
		Next *string          `json:"next"`
	}
	mustDecode(t, rec.Body.Bytes(), &got)
	if got.Next == nil || *got.Next != "cur-2" || len(got.Data) != 2 {
		t.Fatalf("%s", rec.Body.String())
	}
	row := got.Data[0]
	for k, want := range map[string]any{"id": "id-1", "pool": "promo", "delta": float64(-660), "operation_type": "query", "idem_key": "evt-1", "rate_card_version": "v1",
		"seq": float64(7), "event_count": float64(1), "resource": "llm.chat", "rate_key": "gpt", "created_at": "2026-09-29T12:00:00.5Z", "occurred_at": "2026-09-29T12:00:00.5Z"} {
		if row[k] != want {
			t.Fatalf("%s = %v, want %v in %v", k, row[k], want, row)
		}
	}
	if subj, _ := row["subjects"].(map[string]any); subj["user"] != "u1" {
		t.Fatalf("subjects: %v", row["subjects"])
	}
	if ref, _ := row["attributes"].(map[string]any); ref["trace_id"] != "t" {
		t.Fatalf("attributes: %v", row["attributes"])
	}
	if cpj, _ := got.Data[1]["counterparty"].(map[string]any); cpj["id"] != "org-1" || cpj["kind"] != "organization" {
		t.Fatalf("counterparty: %v", got.Data[1]["counterparty"])
	}
	if _, has := got.Data[1]["rate_card_version"]; has {
		t.Fatalf("a non-priced row names no card: %v", got.Data[1])
	}
}

func TestTheLastPageHasNoNextCursor(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{ledger: &stubReads{}})
	rec := a.do("record", ledgerRoute(""))
	var raw map[string]json.RawMessage
	mustDecode(t, rec.Body.Bytes(), &raw)
	if rec.Code != 200 || string(raw["next"]) != "null" || string(raw["data"]) != "[]" {
		t.Fatalf("an empty ledger is an empty list, and the end is a null cursor: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAMembersLedgerAsksOnlyForThatMember(t *testing.T) {
	t.Parallel()
	reads := &stubReads{}
	a := newAPI(t, &stubMeter{}, apiOpts{ledger: reads})
	if rec := a.do("record", ledgerRoute("&subject_kind=user&subject_id=u1")); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if q := reads.gotQ; q.SubjectKind != "user" || q.SubjectID != "u1" {
		t.Fatalf("%+v", q)
	}
	for _, half := range []string{"&subject_kind=user", "&subject_id=u1"} {
		reads.queries = 0
		rec := a.do("record", ledgerRoute(half))
		if rec.Code != 400 || reads.queries != 0 {
			t.Fatalf("half a member would be read as everyone: %s → %d (reads %d)", half, rec.Code, reads.queries)
		}
	}
}

func TestABadLimitOrCursorIsRefusedBeforeAnyRead(t *testing.T) {
	t.Parallel()
	for name, q := range map[string]string{
		"limit zero": "&limit=0", "limit negative": "&limit=-5", "limit past the maximum": "&limit=201", "limit not a number": "&limit=ten", "limit a fraction": "&limit=1.5",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reads := &stubReads{}
			rec := newAPI(t, &stubMeter{}, apiOpts{ledger: reads}).do("record", ledgerRoute(q))
			if rec.Code != 400 || decodeErr(t, rec).Error.Code != "invalid_request" || !strings.Contains(decodeErr(t, rec).Error.Message, "limit") || reads.queries != 0 {
				t.Fatalf("%d %s (reads %d)", rec.Code, rec.Body.String(), reads.queries)
			}
		})
	}
	t.Run("a cursor the reader refuses is a 400", func(t *testing.T) {
		t.Parallel()
		reads := &stubReads{rows: func(context.Context, ports.LedgerQuery) (ports.LedgerPage, error) {
			return ports.LedgerPage{}, errors.Join(domain.ErrInvalid, errors.New("cursor"))
		}}
		if rec := newAPI(t, &stubMeter{}, apiOpts{ledger: reads}).do("record", ledgerRoute("&cursor=garbage")); rec.Code != 400 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("a missing scope", func(t *testing.T) {
		t.Parallel()
		reads := &stubReads{}
		rec := newAPI(t, &stubMeter{}, apiOpts{ledger: reads}).call("record", "GET", "/v1/ledger?scope_kind=workspace", "")
		if rec.Code != 400 || reads.queries != 0 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("a foreign realm", func(t *testing.T) {
		t.Parallel()
		reads := &stubReads{}
		rec := newAPI(t, &stubMeter{}, apiOpts{ledger: reads}).do("record", ledgerRoute("&scope_realm=globex"))
		if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" || reads.queries != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a reader that fails is the table's answer", func(t *testing.T) {
		t.Parallel()
		reads := &stubReads{rows: func(context.Context, ports.LedgerQuery) (ports.LedgerPage, error) {
			return ports.LedgerPage{}, errors.New("pq: connection to db.internal:5432 refused")
		}}
		a := newAPI(t, &stubMeter{}, apiOpts{ledger: reads})
		rec := a.do("record", ledgerRoute(""))
		if rec.Code != 500 || strings.Contains(rec.Body.String(), "db.internal") {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
}

func transferRoute2(key, query string) route {
	return route{"transfer status", "GET", "/v1/transfers/" + url.PathEscape(key) + query, "", false}
}

func TestATransferReadsItsStatusFromTheBooks(t *testing.T) {
	t.Parallel()
	created := mustTime("2026-09-29T12:00:00Z")
	settled := mustTime("2026-09-29T12:00:01Z")
	rec := func(status string, settledAt *time.Time) *stubReads {
		return &stubReads{xfer: func(_ context.Context, realm domain.Realm, key string) (ports.TransferRecord, bool, error) {
			return ports.TransferRecord{
				Realm: realm, IdemKey: key, From: domain.Scope{Realm: realm, Kind: "organization", ID: "org-1"}, To: domain.Scope{Realm: realm, Kind: "workspace", ID: "w2"},
				FromPool: "promo", ToPool: "general", Amount: 2000, Status: status, CreatedAt: created, SettledAt: settledAt,
			}, true, nil
		}}
	}
	for name, tc := range map[string]struct {
		reads  *stubReads
		status string
		has    bool
	}{
		"in transit": {rec(domain.TransferInTransit, nil), "in_transit", false},
		"settled":    {rec(domain.TransferSettled, &settled), "settled", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newAPI(t, &stubMeter{}, apiOpts{transfers: tc.reads})
			rr := a.do("grant", transferRoute2("alloc/1 é", ""))
			if rr.Code != 200 {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			if tc.reads.gotKey != "alloc/1 é" || tc.reads.gotRlm != "acme" {
				t.Fatalf("the books were asked for %q in realm %q", tc.reads.gotKey, tc.reads.gotRlm)
			}
			var m map[string]any
			mustDecode(t, rr.Body.Bytes(), &m)
			if m["status"] != tc.status || m["idem_key"] != "alloc/1 é" || m["amount"] != float64(2000) || m["from_pool"] != "promo" || m["to_pool"] != "general" || m["created_at"] != "2026-09-29T12:00:00Z" {
				t.Fatalf("%v", m)
			}
			if from, _ := m["from"].(map[string]any); from["id"] != "org-1" || from["realm"] != "acme" {
				t.Fatalf("from: %v", m["from"])
			}
			if _, has := m["settled_at"]; has != tc.has {
				t.Fatalf("settled_at present = %v, want %v: %v", has, tc.has, m)
			}
		})
	}
}

// A transfer is accepted in Redis first, and the books learn of it when the writer reaches it: until then
// the answer is "not yet", which is 404 — and the message says why, so a poller keeps polling.
func TestATransferTheBooksHaveNotSeenIsNotFoundYet(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{transfers: &stubReads{}})
	rec := a.do("grant", transferRoute2("alloc-9", ""))
	e := decodeErr(t, rec)
	if rec.Code != 404 || e.Error.Code != "not_found" || !strings.Contains(e.Error.Message, "yet") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestATransferStatusIsRealmBound(t *testing.T) {
	t.Parallel()
	reads := &stubReads{}
	a := newAPI(t, &stubMeter{}, apiOpts{transfers: reads})
	rec := a.do("grant", transferRoute2("alloc-1", "?scope_realm=globex"))
	if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" || reads.queries != 0 {
		t.Fatalf("%d %s (reads %d)", rec.Code, rec.Body.String(), reads.queries)
	}
	if rec := a.do("two", transferRoute2("alloc-1", "")); rec.Code != 400 {
		t.Fatalf("two realms and none named is ambiguous: %d", rec.Code)
	}
	if rec := a.do("two", transferRoute2("alloc-1", "?scope_realm=globex")); rec.Code != 404 || reads.gotRlm != "globex" {
		t.Fatalf("a named realm in the set is read: %d realm=%q", rec.Code, reads.gotRlm)
	}
}

func TestTheReadRoutesExistOnlyWhenTheirReadersDo(t *testing.T) {
	t.Parallel()
	a := newAPI(t, &stubMeter{}, apiOpts{})
	for _, r := range []route{ledgerRoute(""), transferRoute2("k", "")} {
		if rec := a.do("all", r); rec.Code != 404 {
			t.Fatalf("%s is registered with no reader behind it: %d", r.path, rec.Code)
		}
	}
}
