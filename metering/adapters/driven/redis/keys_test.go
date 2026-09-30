package redis

import (
	"strings"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// crc16 (XMODEM) and the hash-tag rule, as Redis Cluster computes a key's slot. Reimplemented here
// on purpose: the assertion is that the KEY LAYOUT lands every key of a scope in one slot, and it
// must not depend on the client library's own idea of it.
func slot(key string) int {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		if j := strings.IndexByte(key[i+1:], '}'); j > 0 {
			key = key[i+1 : i+1+j]
		}
	}
	var crc uint16
	for _, b := range []byte(key) {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return int(crc) % 16384
}

func TestSlotMatchesRedisForKnownKeys(t *testing.T) {
	t.Parallel()
	// Values from `CLUSTER KEYSLOT` on Redis 7.
	for key, want := range map[string]int{"foo": 12182, "{user1000}.following": 3443, "{user1000}.followers": 3443, "somekey": 11058} {
		if got := slot(key); got != want {
			t.Errorf("slot(%q) = %d, want %d", key, got, want)
		}
	}
}

// SC-015 / FR-017f at the unit level: every key an operation touches for a scope — meta, account,
// guard, every counter, the outbox stream — shares ONE slot, because the shard tag is the only
// hash tag in any of them.
func TestEveryKeyOfAScopeSharesOneSlot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	limits := []domain.Limit{
		{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily, TZ: "Asia/Ho_Chi_Minh"},
		{Name: "burst", Unit: domain.CreditUnit, Max: 100, Window: domain.Rolling, Dur: time.Hour},
		{Name: "calls", Unit: "api_call", Max: 5, Window: domain.Hourly},
	}
	// Scope ids that try to smuggle a hash tag in.
	for _, id := range []string{"w1", "{evil}", "a}b{c", "{s0}", "x{"} {
		sc := domain.Scope{Realm: "t1", Kind: "workspace", ID: id}
		for _, shards := range []int{1, 16, 256, 16384} {
			sh := sc.Shard(shards)
			c := domain.Charge{Scope: sc, Amount: 10, IdemKey: "k", Resource: "llm.chat",
				Subjects: domain.Subjects{"user": "u{1}", "job": "j}2"}, Quantities: []domain.Quantity{{Unit: "api_call", Amount: 1}}}
			incs, err := planIncrements(sh, limits, c, now)
			if err != nil {
				t.Fatal(err)
			}
			keys := []string{metaKey(sh), outboxKey(sh), acctKey(sh, sc),
				idemKey(sh, sc, idemUsage, "k{x}"), idemKey(sh, sc, idemTransfer, "k")}
			for _, in := range incs {
				keys = append(keys, in.key)
			}
			reads, err := planReads(sh, sc, limits, domain.AdmitRequest{Resource: "llm.chat", Subjects: c.Subjects}, now)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range reads {
				keys = append(keys, r.keys...)
			}
			want := slot(keys[0])
			for _, k := range keys {
				if got := slot(k); got != want {
					t.Fatalf("scope id %q over %d shards: %q is in slot %d, %q in %d — a script over both is CROSSSLOT",
						id, shards, k, got, keys[0], want)
				}
			}
		}
	}
}

func TestPlanIncrements(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	sc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	sh := sc.Shard(256)
	limits := []domain.Limit{
		{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance},                                           // no counter
		{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily},                     // per user
		{Name: "chat_only", Resource: "llm.chat", Unit: domain.CreditUnit, Max: 100, Window: domain.Daily},                 // only chat
		{Name: "calls", Unit: "api_call", Max: 5, Window: domain.Hourly},                                                   // metered unit
		{Name: "run_cap", Subject: domain.SubjectJob, Unit: domain.CreditUnit, Max: 5, Window: domain.Job, Dur: time.Hour}, // job: automatic, not here
	}
	count := func(c domain.Charge) map[string]int64 {
		incs, err := planIncrements(sh, limits, c, now)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]int64{}
		for _, in := range incs {
			out[in.key] = in.by
		}
		return out
	}

	got := count(domain.Charge{Scope: sc, Amount: 40, Resource: "storage", Subjects: domain.Subjects{"user": "u1"},
		Quantities: []domain.Quantity{{Unit: "api_call", Amount: 2}}})
	if len(got) != 2 {
		t.Fatalf("a storage call by u1 counts the user's daily and the call counter, not the chat-only one nor a balance: %v", got)
	}
	for k, v := range got {
		switch {
		case strings.Contains(k, ":user_daily:u1:"):
			if v != 40 {
				t.Errorf("user_daily += %d, want the charge's credits", v)
			}
		case strings.Contains(k, ":calls:-:"):
			if v != 2 {
				t.Errorf("calls += %d, want the quantity", v)
			}
		default:
			t.Errorf("unexpected counter %q", k)
		}
	}

	if n := len(count(domain.Charge{Scope: sc, Amount: 40, Resource: "llm.chat", Subjects: domain.Subjects{"user": "u1"}})); n != 2 {
		t.Fatalf("a chat call counts the user's daily and the chat-only limit (no api_call quantity, so no call counter): %d", n)
	}
	if got := count(domain.Charge{Scope: sc, Amount: 40}); len(got) != 0 {
		t.Fatalf("no subject and no resource: the user limit cannot be counted, the chat-only one does not govern it: %v", got)
	}
	if got := count(domain.Charge{Scope: sc, Amount: 0, Resource: "storage"}); len(got) != 0 {
		t.Fatalf("a zero increment writes no key: %v", got)
	}

	// A job counter is automatic — whenever the charge carries a job — in credits and every unit.
	got = count(domain.Charge{Scope: sc, Amount: 40, Subjects: domain.Subjects{"job": "r1"},
		Quantities: []domain.Quantity{{Unit: "tokens", Amount: 7}, {Unit: "tokens", Amount: 3}}})
	credits, tokens := int64(-1), int64(-1)
	for k, v := range got {
		if strings.Contains(k, ":job.credit:r1:-") {
			credits = v
		}
		if strings.Contains(k, ":job.tokens:r1:-") {
			tokens = v
		}
	}
	if credits != 40 || tokens != 10 {
		t.Fatalf("job counters: credits=%d tokens=%d in %v", credits, tokens, got)
	}
}

func TestPlanReadsRefusesAMissingSubject(t *testing.T) {
	t.Parallel()
	sc := domain.Scope{Realm: "t1", Kind: "workspace", ID: "w1"}
	l := domain.Limit{Name: "user_daily", Subject: "user", Unit: domain.CreditUnit, Max: 1, Window: domain.Daily}
	_, err := planReads(0, sc, []domain.Limit{l}, domain.AdmitRequest{}, time.Now())
	if err == nil {
		t.Fatal("a ceiling that silently does not apply is the worst kind")
	}
}
