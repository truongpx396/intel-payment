package resthandler_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// stubAdmin is a programmable ports.Admin.
type stubAdmin struct {
	mu      sync.Mutex
	err     error
	created bool
	calls   int

	change  ports.ConfigChange
	realm   domain.Realm
	pub     ports.CardPublication
	limit   ports.LimitConfig
	pool    ports.PoolConfig
	deleted string
}

var _ ports.Admin = (*stubAdmin)(nil)

func (a *stubAdmin) touch(ch ports.ConfigChange, r domain.Realm) {
	a.mu.Lock()
	a.calls++
	a.change, a.realm = ch, r
	a.mu.Unlock()
}

func (a *stubAdmin) PublishRateCard(_ context.Context, ch ports.ConfigChange, r domain.Realm, p ports.CardPublication) (ports.RateCardInfo, error) {
	a.touch(ch, r)
	a.pub = p
	card, _ := domain.NewRateCard(r, p.Version, p.Pricer, p.Entries, nil)
	return ports.RateCardInfo{Card: card, PublishedAt: mustTime("2026-10-01T00:00:00Z")}, a.err
}

func (a *stubAdmin) RateCards(_ context.Context, r domain.Realm) ([]ports.RateCardInfo, error) {
	a.touch(ports.ConfigChange{}, r)
	old, _ := domain.NewRateCard(r, "v1", "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150_000, BlockSize: 1_000_000, CostMicrosPerBlock: 100_000}}, nil)
	retired := mustTime("2026-09-30T00:00:00Z")
	cur, _ := domain.NewRateCard(r, "v2", "table", []domain.RateEntry{{RateKey: "gpt", Unit: "in", CreditsPerBlock: 300_000, BlockSize: 1_000_000}}, nil)
	return []ports.RateCardInfo{
		{Card: old, PublishedAt: mustTime("2026-01-01T00:00:00Z"), RetiredAt: &retired},
		{Card: cur, PublishedAt: mustTime("2026-09-30T00:00:00Z")},
	}, a.err
}

func (a *stubAdmin) PutLimit(_ context.Context, ch ports.ConfigChange, r domain.Realm, l ports.LimitConfig) (bool, error) {
	a.touch(ch, r)
	a.limit = l
	return a.created, a.err
}

func (a *stubAdmin) Limit(_ context.Context, r domain.Realm, name string) (ports.LimitConfig, error) {
	a.touch(ports.ConfigChange{}, r)
	ten := 10
	return ports.LimitConfig{Limit: domain.Limit{Name: name, Subject: "user", Resource: "llm.chat", Unit: domain.CreditUnit, Max: 100, MaxEntitlement: "daily_user_credits",
		Window: domain.Daily, TZ: "UTC", WarnAt: 0.8, DenyCode: domain.DenyLimitReached}, SortOrder: &ten}, a.err
}

func (a *stubAdmin) DeleteLimit(_ context.Context, ch ports.ConfigChange, r domain.Realm, name string) error {
	a.touch(ch, r)
	a.deleted = name
	return a.err
}

func (a *stubAdmin) PutPool(_ context.Context, ch ports.ConfigChange, r domain.Realm, p ports.PoolConfig) (bool, error) {
	a.touch(ch, r)
	a.pool = p
	return a.created, a.err
}

func (a *stubAdmin) Pool(_ context.Context, r domain.Realm, name string) (ports.PoolConfig, error) {
	a.touch(ports.ConfigChange{}, r)
	return ports.PoolConfig{Pool: domain.PoolDef{Name: domain.Pool(name), Priority: 10, AppliesTo: []string{"gpu.job"}}, Description: "gpu credits"}, a.err
}

func adminCall(a *api, token, method, path, body string) *httptest.ResponseRecorder {
	return a.call(token, method, path, body)
}

const cardBody = `{"actor":"alice@example.com","reason":"cheaper tokens","version":"2026-10-01","pricer":"table","retire_previous":true,` +
	`"entries":[{"rate_key":"gpt","unit":"in","credits_per_block":150000,"block_size":1000000,"cost_micros_per_block":100000},{"rate_key":"gpt","unit":"out","credits_per_block":600000,"block_size":1000000}]}`

func TestPublishingACardCarriesWhoWhyAndWhichRealm(t *testing.T) {
	t.Parallel()
	adm := &stubAdmin{}
	a := newAPI(t, &stubMeter{}, apiOpts{admin: adm})
	rec := adminCall(a, "admin", "POST", "/v1/admin/rate-cards?realm=globex", cardBody)
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if adm.change.Actor != "alice@example.com" || adm.change.Reason != "cheaper tokens" || adm.change.Via != "ops-tool" || adm.realm != "globex" {
		t.Fatalf("who, why, through which credential, and in which realm: %+v %q", adm.change, adm.realm)
	}
	p := adm.pub
	if p.Version != "2026-10-01" || p.Pricer != "table" || !p.RetirePrevious || len(p.Entries) != 2 ||
		p.Entries[0] != (domain.RateEntry{RateKey: "gpt", Unit: "in", CreditsPerBlock: 150_000, BlockSize: 1_000_000, CostMicrosPerBlock: 100_000}) {
		t.Fatalf("%+v", p)
	}
	var got struct {
		Version   string `json:"version"`
		Pricer    string `json:"pricer"`
		Published bool   `json:"published"`
		Entries   []struct {
			RateKey string `json:"rate_key"`
			Credits int64  `json:"credits_per_block"`
		} `json:"entries"`
	}
	mustDecode(t, rec.Body.Bytes(), &got)
	if got.Version != "2026-10-01" || got.Pricer != "table" || len(got.Entries) != 2 || got.Entries[0].Credits != 150_000 {
		t.Fatalf("%s", rec.Body.String())
	}
	// retire_previous is explicit: leaving it out does not retire the card every Record is priced under.
	adm2 := &stubAdmin{}
	body := strings.Replace(cardBody, `"retire_previous":true,`, "", 1)
	if rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm2}), "admin", "POST", "/v1/admin/rate-cards?realm=acme", body); rec.Code != 201 || adm2.pub.RetirePrevious {
		t.Fatalf("%d retire=%v", rec.Code, adm2.pub.RetirePrevious)
	}
}

func TestAWriteWithoutAPersonAndAReasonIsRefusedBeforeItIsMade(t *testing.T) {
	t.Parallel()
	limitBody := func(extra string) string {
		return `{` + extra + `"unit":"credit","max":10,"window":"daily","tz":"UTC","warn_at":0.8,"deny_code":"limit_reached"}`
	}
	writes := map[string]func(extra string) (method, path, body string){
		"publish a card": func(extra string) (string, string, string) {
			return "POST", "/v1/admin/rate-cards?realm=acme", `{` + extra + `"version":"v2","entries":[{"rate_key":"gpt","unit":"in","credits_per_block":1,"block_size":1}]}`
		},
		"put a limit": func(extra string) (string, string, string) {
			return "PUT", "/v1/admin/limits/user_daily?realm=acme", limitBody(extra)
		},
		"put a pool": func(extra string) (string, string, string) {
			return "PUT", "/v1/admin/pools/promo?realm=acme", `{` + extra + `"priority":10}`
		},
	}
	for name, mk := range writes {
		for missing, extra := range map[string]string{
			"no actor":  `"reason":"because",`,
			"no reason": `"actor":"alice",`,
			"neither":   ``,
			"blank":     `"actor":" ","reason":"\t",`,
		} {
			t.Run(name+"/"+missing, func(t *testing.T) {
				t.Parallel()
				adm := &stubAdmin{}
				method, path, body := mk(extra)
				rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm}), "admin", method, path, body)
				e := decodeErr(t, rec)
				if rec.Code != 400 || e.Error.Code != "invalid_request" || (!strings.Contains(e.Error.Message, "actor") && !strings.Contains(e.Error.Message, "reason")) || adm.calls != 0 {
					t.Fatalf("%d %s (calls %d)", rec.Code, rec.Body.String(), adm.calls)
				}
			})
		}
	}
	t.Run("delete a limit: the person and the reason are query parameters", func(t *testing.T) {
		t.Parallel()
		for _, q := range []string{"", "&actor=alice", "&reason=because", "&actor=%20&reason=because"} {
			adm := &stubAdmin{}
			rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm}), "admin", "DELETE", "/v1/admin/limits/user_daily?realm=acme"+q, "")
			if rec.Code != 400 || adm.calls != 0 {
				t.Fatalf("%q: %d (calls %d)", q, rec.Code, adm.calls)
			}
		}
	})
}

func TestLimitsAndPoolsAreReadAndWrittenByName(t *testing.T) {
	t.Parallel()
	t.Run("put a limit: created is 201, updated is 200", func(t *testing.T) {
		t.Parallel()
		body := `{"actor":"alice","reason":"free tier","subject_kind":"user","resource":"llm.chat","unit":"credit","max":10000000,"max_entitlement":"daily_user_credits",` +
			`"window":"daily","tz":"Asia/Ho_Chi_Minh","warn_at":0.8,"deny_code":"limit_reached","sort_order":20}`
		for created, want := range map[bool]int{true: 201, false: 200} {
			adm := &stubAdmin{created: created}
			rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm}), "admin", "PUT", "/v1/admin/limits/user_daily?realm=acme", body)
			if rec.Code != want {
				t.Fatalf("created=%v: %d %s", created, rec.Code, rec.Body.String())
			}
			l := adm.limit
			if l.Limit.Name != "user_daily" || l.Limit.Subject != "user" || l.Limit.Resource != "llm.chat" || l.Limit.Max != 10_000_000 || l.Limit.MaxEntitlement != "daily_user_credits" ||
				l.Limit.Window != domain.Daily || l.Limit.TZ != "Asia/Ho_Chi_Minh" || l.Limit.WarnAt != 0.8 || l.Limit.DenyCode != "limit_reached" || l.SortOrder == nil || *l.SortOrder != 20 {
				t.Fatalf("%+v", l)
			}
			if adm.change.Actor != "alice" || adm.change.Reason != "free tier" || adm.realm != "acme" {
				t.Fatalf("%+v", adm.change)
			}
		}
	})
	t.Run("a limit's window is required: a default would make it a balance", func(t *testing.T) {
		t.Parallel()
		adm := &stubAdmin{}
		rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm}), "admin", "PUT", "/v1/admin/limits/x?realm=acme", `{"actor":"a","reason":"r","unit":"credit","max":1,"deny_code":"limit_reached"}`)
		if rec.Code != 400 || !strings.Contains(decodeErr(t, rec).Error.Message, "window") || adm.calls != 0 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("get a limit", func(t *testing.T) {
		t.Parallel()
		rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: &stubAdmin{}}), "admin", "GET", "/v1/admin/limits/user_daily?realm=acme", "")
		var m map[string]any
		mustDecode(t, rec.Body.Bytes(), &m)
		for k, want := range map[string]any{"name": "user_daily", "subject_kind": "user", "resource": "llm.chat", "unit": "credit", "max": float64(100), "max_entitlement": "daily_user_credits",
			"window": "daily", "tz": "UTC", "warn_at": 0.8, "deny_code": "limit_reached", "sort_order": float64(10)} {
			if rec.Code != 200 || m[k] != want {
				t.Fatalf("%s = %v, want %v (%d %s)", k, m[k], want, rec.Code, rec.Body.String())
			}
		}
	})
	t.Run("delete a limit", func(t *testing.T) {
		t.Parallel()
		adm := &stubAdmin{}
		rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: adm}), "admin", "DELETE", "/v1/admin/limits/user_daily?realm=acme&actor=alice&reason=retired+the+tier", "")
		if rec.Code != 204 || rec.Body.Len() != 0 || adm.deleted != "user_daily" || adm.change.Actor != "alice" || adm.change.Reason != "retired the tier" {
			t.Fatalf("%d %q %+v", rec.Code, rec.Body.String(), adm.change)
		}
	})
	t.Run("put and get a pool", func(t *testing.T) {
		t.Parallel()
		adm := &stubAdmin{created: true}
		a := newAPI(t, &stubMeter{}, apiOpts{admin: adm})
		rec := adminCall(a, "admin", "PUT", "/v1/admin/pools/gpu?realm=acme", `{"actor":"alice","reason":"gpu credits","priority":5,"applies_to":["gpu.job","gpu.batch"],"description":"for gpu work"}`)
		if rec.Code != 201 || adm.pool.Pool.Name != "gpu" || adm.pool.Pool.Priority != 5 || len(adm.pool.Pool.AppliesTo) != 2 || adm.pool.Description != "for gpu work" {
			t.Fatalf("%d %s %+v", rec.Code, rec.Body.String(), adm.pool)
		}
		var m map[string]any
		get := adminCall(a, "admin", "GET", "/v1/admin/pools/gpu?realm=acme", "")
		mustDecode(t, get.Body.Bytes(), &m)
		if get.Code != 200 || m["name"] != "gpu" || m["priority"] != float64(10) || m["description"] != "gpu credits" {
			t.Fatalf("%d %s", get.Code, get.Body.String())
		}
	})
}

func TestTheRateCardsListSaysWhichIsActive(t *testing.T) {
	t.Parallel()
	rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{admin: &stubAdmin{}}), "admin", "GET", "/v1/admin/rate-cards?realm=acme", "")
	var got struct {
		Data []struct {
			Version     string `json:"version"`
			Active      bool   `json:"active"`
			PublishedAt string `json:"published_at"`
			RetiredAt   string `json:"retired_at"`
			Entries     []struct {
				Cost *int64 `json:"cost_micros_per_block"`
			} `json:"entries"`
		} `json:"data"`
	}
	mustDecode(t, rec.Body.Bytes(), &got)
	if rec.Code != 200 || len(got.Data) != 2 || got.Data[0].Version != "v1" || got.Data[0].Active || got.Data[0].RetiredAt != "2026-09-30T00:00:00Z" ||
		got.Data[1].Version != "v2" || !got.Data[1].Active || got.Data[1].RetiredAt != "" || got.Data[0].Entries[0].Cost == nil || *got.Data[0].Entries[0].Cost != 100_000 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestAdminErrorsAreTheTables(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		status int
		code   string
	}{
		"a card that cannot price":    {domain.ErrUnpriceable, 422, "unpriceable"},
		"a card that overflows":       {domain.ErrAmountOutOfRange, 422, "amount_out_of_range"},
		"a version already published": {domain.ErrAlreadyExists, 409, "already_exists"},
		"a limit that is not there":   {domain.ErrNotFound, 404, "not_found"},
		"a shape the rules refuse":    {domain.ErrInvalid, 400, "invalid_request"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a := newAPI(t, &stubMeter{}, apiOpts{admin: &stubAdmin{err: tc.err}})
			for _, r := range []struct{ method, path, body string }{
				{"POST", "/v1/admin/rate-cards?realm=acme", cardBody},
				{"PUT", "/v1/admin/limits/x?realm=acme", `{"actor":"a","reason":"r","unit":"credit","max":1,"window":"daily","deny_code":"limit_reached"}`},
				{"GET", "/v1/admin/limits/x?realm=acme", ""},
				{"DELETE", "/v1/admin/limits/x?realm=acme&actor=a&reason=r", ""},
				{"PUT", "/v1/admin/pools/x?realm=acme", `{"actor":"a","reason":"r","priority":1}`},
				{"GET", "/v1/admin/pools/x?realm=acme", ""},
			} {
				rec := adminCall(a, "admin", r.method, r.path, r.body)
				if rec.Code != tc.status || decodeErr(t, rec).Error.Code != tc.code {
					t.Fatalf("%s %s: %d %s", r.method, r.path, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestAdminIsRealmBoundAndOnlyForAdmin(t *testing.T) {
	t.Parallel()
	adm := &stubAdmin{}
	a := newAPI(t, &stubMeter{}, apiOpts{admin: adm})
	t.Run("a realm outside the credential", func(t *testing.T) {
		t.Parallel()
		rec := adminCall(a, "admin", "GET", "/v1/admin/limits/x?realm=initech", "")
		if rec.Code != 403 || decodeErr(t, rec).Error.Code != "realm_mismatch" {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("two realms and none named: ambiguous", func(t *testing.T) {
		t.Parallel()
		if rec := adminCall(a, "admin", "GET", "/v1/admin/limits/x", ""); rec.Code != 400 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("record and grant are not admin", func(t *testing.T) {
		t.Parallel()
		for _, token := range []string{"record", "grant", "all"} {
			if rec := adminCall(a, token, "POST", "/v1/admin/rate-cards?realm=acme", cardBody); rec.Code != 403 || decodeErr(t, rec).Error.Code != "forbidden" {
				t.Fatalf("%s: %d", token, rec.Code)
			}
		}
	})
	t.Run("the routes exist only with an admin service", func(t *testing.T) {
		t.Parallel()
		if rec := adminCall(newAPI(t, &stubMeter{}, apiOpts{}), "admin", "GET", "/v1/admin/rate-cards?realm=acme", ""); rec.Code != 404 {
			t.Fatalf("%d", rec.Code)
		}
	})
}
