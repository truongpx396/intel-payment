package contracts

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/truongpx396/intel-payment/metering/ports"
)

// MemClaimerDefects are the deliberate faults a MemClaimer can be given; ClaimerContract is shown to reject each by name.
type MemClaimerDefects struct {
	NoExclusion    bool // every claim succeeds
	OneLockForAll  bool // all keys are one lock
	ReleaseNothing bool // a release gives nothing back
	ReleaseFrees   bool // a second release frees the claim a LATER holder took
	DeadKeepsClaim bool // a holder that dies keeps its claim
	IgnoresContext bool // a cancelled context still claims
}

// MemClaimer is an in-memory ports.Claimer: the honest reference ClaimerContract is written against, and what the tick framework's
// tests run on.
type MemClaimer struct {
	Defects MemClaimerDefects

	mu    sync.Mutex
	held  map[string]int // key → the token of the holder
	token int
}

var _ ports.Claimer = (*MemClaimer)(nil)

// NewMemClaimer builds an empty claimer.
func NewMemClaimer() *MemClaimer { return &MemClaimer{held: map[string]int{}} }

func (m *MemClaimer) TryClaim(ctx context.Context, key string) (func(), bool, error) {
	if !m.Defects.IgnoresContext {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key
	if m.Defects.OneLockForAll {
		k = ""
	}
	if _, taken := m.held[k]; taken && !m.Defects.NoExclusion {
		return func() {}, false, nil
	}
	m.token++
	mine := m.token
	m.held[k] = mine
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.Defects.ReleaseNothing {
			return
		}
		if tok, ok := m.held[k]; ok && (tok == mine || m.Defects.ReleaseFrees) {
			delete(m.held, k)
		}
	}, true, nil
}

// Kill is the holder of key dying: a killed process holds nothing, whatever it had not got round to releasing.
func (m *MemClaimer) Kill(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.Defects.DeadKeepsClaim {
		delete(m.held, key)
	}
}

// ClaimerBackend is a claimer under test and the way to kill a holder of one of its claims.
type ClaimerBackend struct {
	Claims ports.Claimer
	// Kill ends the life of whoever holds key, without letting it release anything: the connection drops, the process is killed.
	Kill func(key string)
}

// ClaimerFactory builds a fresh claimer.
type ClaimerFactory func(tb testing.TB) ClaimerBackend

// ClaimerCase is one named obligation of the claim.
type ClaimerCase struct {
	Name string
	Run  func(tb testing.TB, f ClaimerFactory)
}

// The names of the cases, so a test that plants a defect can say which case must catch it.
const (
	CaseClaimExclusive   = "a claim is held by one run at a time"
	CaseClaimIndependent = "claims on different keys do not exclude each other"
	CaseClaimReleased    = "a released claim can be taken at once, and releasing twice is harmless to the next holder"
	CaseClaimTogether    = "sixteen runs at once: exactly one holds the claim"
	CaseClaimDeadHolder  = "a holder that dies loses the claim, and the next run takes the work over"
	CaseClaimCancelled   = "a run whose context is already cancelled claims nothing"
)

// ClaimerCases are the obligations of a ports.Claimer.
func ClaimerCases() []ClaimerCase {
	return []ClaimerCase{
		{CaseClaimExclusive, claimExclusive}, {CaseClaimIndependent, claimIndependent}, {CaseClaimReleased, claimReleased},
		{CaseClaimTogether, claimTogether}, {CaseClaimDeadHolder, claimDeadHolder}, {CaseClaimCancelled, claimCancelled},
	}
}

// ClaimerContract runs every case, each against a claimer of its own, in parallel.
func ClaimerContract(t *testing.T, f ClaimerFactory) {
	t.Helper()
	for _, c := range ClaimerCases() {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			c.Run(t, f)
		})
	}
}

func mustClaim(tb testing.TB, c ports.Claimer, key string) func() {
	tb.Helper()
	release, ok, err := c.TryClaim(context.Background(), key)
	if err != nil || !ok {
		tb.Fatalf("claim %q: ok %v, %v", key, ok, err)
	}
	return release
}

func claimExclusive(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	release := mustClaim(tb, b.Claims, "reconcile|3")
	defer release()
	if _, ok, err := b.Claims.TryClaim(context.Background(), "reconcile|3"); err != nil || ok {
		tb.Fatalf("a second run took a claim that is held: ok %v, %v", ok, err)
	}
}

func claimIndependent(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	a := mustClaim(tb, b.Claims, "reconcile|3")
	defer a()
	other := mustClaim(tb, b.Claims, "reconcile|4")
	defer other()
	third := mustClaim(tb, b.Claims, "audit|3")
	defer third()
}

func claimReleased(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	first := mustClaim(tb, b.Claims, "journal|")
	first()
	second := mustClaim(tb, b.Claims, "journal|") // taken at once, not after a timeout
	first()                                       // the first holder releases again, late: it must not free the second's claim
	if _, ok, _ := b.Claims.TryClaim(context.Background(), "journal|"); ok {
		tb.Fatal("a late second release freed the claim a later holder took")
	}
	second()
	third := mustClaim(tb, b.Claims, "journal|")
	third()
}

func claimTogether(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		won  int
		held []func()
	)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, ok, err := b.Claims.TryClaim(context.Background(), "expiry|")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				tb.Errorf("TryClaim: %v", err)
				return
			}
			if ok {
				won++
				held = append(held, rel)
			}
		}()
	}
	wg.Wait()
	for _, r := range held {
		r()
	}
	if won != 1 {
		tb.Fatalf("%d of 16 concurrent runs hold the claim, want exactly 1", won)
	}
}

func claimDeadHolder(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	zombie := mustClaim(tb, b.Claims, "transfer|") // held, and never released in time: the process was killed
	if _, ok, _ := b.Claims.TryClaim(context.Background(), "transfer|"); ok {
		tb.Fatal("a live holder lost its claim")
	}
	b.Kill("transfer|")
	deadline := time.Now().Add(5 * time.Second)
	for {
		next, ok, err := b.Claims.TryClaim(context.Background(), "transfer|")
		if err != nil {
			tb.Fatalf("TryClaim: %v", err)
		}
		if ok {
			// If the dead holder turns out to be a slow one and releases now, the run that took over keeps its claim.
			zombie()
			if _, again, _ := b.Claims.TryClaim(context.Background(), "transfer|"); again {
				tb.Fatal("the late release of a holder that had died freed the claim of the run that took over")
			}
			next()
			return
		}
		if time.Now().After(deadline) {
			zombie()
			tb.Fatal("a holder that died still holds the claim, so the work it left half done is never completed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func claimCancelled(tb testing.TB, f ClaimerFactory) {
	b := f(tb)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, ok, err := b.Claims.TryClaim(ctx, "trim|")
	if ok {
		release()
		tb.Fatal("a run that was already cancelled took a claim")
	}
	if err == nil {
		tb.Fatal("a cancelled claim reported neither a claim nor an error")
	}
}
