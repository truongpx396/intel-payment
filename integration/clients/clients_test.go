//go:build clients

package clients_test

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/resthandler"
	"github.com/truongpx396/intel-payment/metering/contracts"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// T063b: the clients generated from api/openapi/v1.yaml compile and round-trip an Admit against the handler.
// The handler is the real resthandler with its real authentication; the engine behind it is a stand-in that
// records what reached it, because what is under test is the document and what is generated from it.

const token = "clients-check-token"

type engine struct {
	mu     sync.Mutex
	admits []admit
}

type admit struct {
	scope domain.Scope
	req   domain.AdmitRequest
}

func (e *engine) Admit(_ context.Context, s domain.Scope, req domain.AdmitRequest) (domain.Admission, error) {
	e.mu.Lock()
	e.admits = append(e.admits, admit{s, req})
	e.mu.Unlock()
	if req.Resource == "spent" {
		return domain.Admission{Exceeded: &domain.Limit{Name: "scope_balance", Unit: domain.CreditUnit, Window: domain.Balance, DenyCode: domain.DenyPaymentRequired}}, nil
	}
	return domain.Admission{Allowed: true, Headroom: 7}, nil
}

func (*engine) Record(context.Context, domain.Event) (domain.Receipt, error) {
	return domain.Receipt{}, domain.ErrInvalid
}
func (*engine) Grant(context.Context, domain.Grant) (domain.Receipt, error) {
	return domain.Receipt{}, domain.ErrInvalid
}
func (*engine) Transfer(context.Context, domain.Transfer) (domain.TransferReceipt, error) {
	return domain.TransferReceipt{}, domain.ErrInvalid
}
func (*engine) Balance(context.Context, domain.Scope) (map[domain.Pool]domain.Credits, error) {
	return map[domain.Pool]domain.Credits{}, nil
}

func TestGeneratedClientsCompileAndRoundTripAnAdmit(t *testing.T) {
	t.Parallel()
	eng := &engine{}
	h, err := resthandler.New(resthandler.Config{
		Meter: eng, Balance: eng, Clock: contracts.NewFakeClock(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)),
		Auth: auth.AuthenticatorFunc(func(_ context.Context, c auth.Credential) (auth.Principal, error) {
			if c.Bearer != token {
				return auth.Principal{}, auth.ErrUnauthenticated
			}
			return auth.Principal{CallerID: "clients-check", AnyRealm: true, Privileges: auth.Record}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	cmd := exec.CommandContext(t.Context(), "../../scripts/verify-clients.sh")
	cmd.Env = append(cmd.Environ(), "BASE_URL="+srv.URL, "TOKEN="+token)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the generated clients do not compile and round-trip an Admit: %v", err)
	}

	// The clients really reached the engine, each with what it was given: two languages, two Admits each.
	eng.mu.Lock()
	defer eng.mu.Unlock()
	if len(eng.admits) != 4 {
		t.Fatalf("the engine saw %d admits, want 4 (an allowed and a refused one from each client)", len(eng.admits))
	}
	for i, a := range eng.admits {
		if a.scope.Kind != "workspace" || a.scope.ID != "w1" {
			t.Errorf("admit %d reached the engine for scope %+v, the clients sent workspace/w1", i, a.scope)
		}
	}
	var allowedWithCost int
	for _, a := range eng.admits {
		if a.req.Resource == "llm.chat" && a.req.MaxCost == 10 {
			allowedWithCost++
		}
	}
	if allowedWithCost != 2 {
		t.Errorf("%d admits carried resource llm.chat and max_cost 10, want 2 (one per client): %+v", allowedWithCost, eng.admits)
	}
	if !strings.Contains(string(out), "typescript:") || !strings.Contains(string(out), "python:") {
		t.Errorf("the round trip did not report both clients:\n%s", out)
	}
}
