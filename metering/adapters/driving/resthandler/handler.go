package resthandler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/truongpx396/intel-payment/metering/adapters/driving/auth"
	"github.com/truongpx396/intel-payment/metering/adapters/driving/wire"
	"github.com/truongpx396/intel-payment/metering/domain"
	"github.com/truongpx396/intel-payment/metering/ports"
)

// Config is what the REST surface is built from. The engine does the work; this decides nothing about money.
type Config struct {
	Meter   ports.Meter
	Balance ports.BalanceReader
	// Ledger and Transfers back GET /v1/ledger and GET /v1/transfers/{idem_key}. A route whose reader is
	// absent is not registered.
	Ledger    ports.LedgerReader
	Transfers ports.TransferReader
	Auth      auth.Authenticator
	Clock     ports.Clock
	Logger    *slog.Logger

	// Legacy429ForLimits answers 429 instead of 409 for a spent window, for a host mid-migration
	// (PAYMENT_LEGACY_429_FOR_LIMITS). 429 means "you are calling too fast"; a spent budget is not a rate
	// problem, which is why 409 is the default.
	Legacy429ForLimits bool
	// UpgradeURL gives the way out of a 402, when a plan exists to buy. nil, or "", omits it.
	UpgradeURL func(ctx context.Context, s domain.Scope) string
	// MaxBodyBytes caps a request body. Default 1 MiB.
	MaxBodyBytes int64
}

func (c Config) validate() error {
	var errs []error
	if c.Meter == nil || c.Balance == nil {
		errs = append(errs, errors.New("a Meter and a BalanceReader are required"))
	}
	if c.Auth == nil {
		errs = append(errs, errors.New("an Authenticator is required: without one the /v1 routes would serve money to anyone"))
	}
	if c.Clock == nil {
		errs = append(errs, errors.New("a Clock is required: a ceiling's reset is told from it"))
	}
	return errors.Join(errs...)
}

// New builds the REST handler: the route table of BuildRegistry behind the registry's authentication.
func New(cfg Config) (http.Handler, error) {
	reg, err := BuildRegistry(cfg)
	if err != nil {
		return nil, err
	}
	return reg.Handler(Options{Auth: cfg.Auth, Logger: cfg.Logger, MaxBodyBytes: cfg.MaxBodyBytes})
}

// BuildRegistry is the production route table, for the checks that read it as data.
func BuildRegistry(cfg Config) (*Registry, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("resthandler: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	h := &handler{cfg: cfg}
	reg := NewRegistry()
	for _, r := range []Route{
		{Method: http.MethodPost, Path: "/v1/admit", Surface: V1, Privilege: auth.Record, Handler: h.admit},
		{Method: http.MethodPost, Path: "/v1/record", Surface: V1, Privilege: auth.Record, Handler: h.record},
		{Method: http.MethodPost, Path: "/v1/grant", Surface: V1, Privilege: auth.Grant, Handler: h.grant},
		{Method: http.MethodPost, Path: "/v1/transfer", Surface: V1, Privilege: auth.Grant, Handler: h.transfer},
		{Method: http.MethodGet, Path: "/v1/balance", Surface: V1, Privilege: auth.Record, Handler: h.balance},
	} {
		reg.Add(r)
	}
	if cfg.Ledger != nil {
		reg.Add(Route{Method: http.MethodGet, Path: "/v1/ledger", Surface: V1, Privilege: auth.Record, Handler: h.ledger})
	}
	if cfg.Transfers != nil {
		reg.Add(Route{Method: http.MethodGet, Path: "/v1/transfers/{idem_key}", Surface: V1, Privilege: auth.Grant, Handler: h.transferStatus})
	}
	return reg, nil
}

type handler struct{ cfg Config }

// caller is who the registry authenticated. Its absence is a wiring bug, and refuses rather than allows.
func (h *handler) caller(w http.ResponseWriter, r *http.Request) (auth.Principal, bool) {
	p, ok := auth.FromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "missing or invalid service credential")
	}
	return p, ok
}

func (h *handler) admit(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req admitRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "admit", err)
		return
	}
	if req.Scope == nil {
		h.fail(w, r, "admit", required("scope"))
		return
	}
	ar := domain.AdmitRequest{Resource: req.Resource, Subjects: subjectsOf(req.Subjects), MaxCost: domain.Credits(req.MaxCost)}
	for i, l := range req.Limits {
		dl, err := limitFromJSON(l, fmt.Sprintf("limits[%d]", i))
		if err != nil {
			h.fail(w, r, "admit", err)
			return
		}
		ar.Limits = append(ar.Limits, dl)
	}
	scope, err := p.Resolve(req.Scope.domain())
	if err != nil {
		h.fail(w, r, "admit", err)
		return
	}
	adm, err := h.cfg.Meter.Admit(r.Context(), scope, ar)
	if err != nil {
		h.fail(w, r, "admit", err)
		return
	}
	if adm.Allowed {
		writeJSON(w, http.StatusOK, admissionToJSON(adm))
		return
	}
	h.deny(w, r, scope, adm)
}

// deny answers a refused Admit with the status the error table gives it. The body is the error envelope
// AND the Admission's own fields, so a client that reads `.error.code` and one that reads `.allowed` both
// find what they look for.
func (h *handler) deny(w http.ResponseWriter, r *http.Request, scope domain.Scope, adm domain.Admission) {
	cond, ok := wire.Denial(adm)
	if !ok {
		h.fail(w, r, "admit", fmt.Errorf("the engine refused an admission without naming the ceiling: %+v", adm))
		return
	}
	status := cond.HTTP
	if cond.Reason == domain.DenyLimitReached && h.cfg.Legacy429ForLimits {
		status = http.StatusTooManyRequests
	}
	details := map[string]any{}
	msg := "The account is held pending an operator."
	if x := adm.Exceeded; x != nil && x.Name != "" {
		details["limit"] = x.Name
	}
	switch cond.Reason {
	case domain.DenyPaymentRequired:
		msg = "Credit balance exhausted."
		if h.cfg.UpgradeURL != nil {
			if u := h.cfg.UpgradeURL(r.Context(), scope); u != "" {
				details["upgrade_url"] = u
			}
		}
	case domain.DenyLimitReached:
		msg = fmt.Sprintf("Limit %q reached.", adm.Exceeded.Name)
		if at, ok := adm.Exceeded.ResetsAt(h.cfg.Clock.Now()); ok {
			details["resets_at"] = at.UTC().Format(time.RFC3339Nano)
		} else {
			details["window"] = adm.Exceeded.Window.String()
			if adm.Exceeded.Dur > 0 {
				details["dur_seconds"] = int64(adm.Exceeded.Dur / time.Second)
			}
		}
	}
	if len(details) == 0 {
		details = nil
	}
	writeJSON(w, status, struct {
		Error errorDetail `json:"error"`
		admissionJSON
	}{errorDetail{Code: cond.HTTPCode, Message: msg, Details: details}, admissionToJSON(adm)})
}

func (h *handler) record(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req recordRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "record", err)
		return
	}
	switch {
	case req.Scope == nil:
		h.fail(w, r, "record", required("scope"))
		return
	case req.OccurredAt == "":
		h.fail(w, r, "record", required("occurred_at"))
		return
	}
	at, err := parseTime(req.OccurredAt, "occurred_at")
	if err != nil {
		h.fail(w, r, "record", err)
		return
	}
	key, err := idemKey(r, req.IdemKey)
	if err != nil {
		h.fail(w, r, "record", err)
		return
	}
	e := domain.Event{
		Resource: req.Resource, RateKey: req.RateKey, Subjects: subjectsOf(req.Subjects), IdemKey: key,
		OccurredAt: at, Attributes: mapOrNil(req.Attributes),
	}
	for _, q := range req.Quantities {
		e.Quantities = append(e.Quantities, domain.Quantity{Unit: domain.Unit(q.Unit), Amount: q.Amount})
	}
	if e.Scope, err = p.Resolve(req.Scope.domain()); err != nil {
		h.fail(w, r, "record", err)
		return
	}
	rc, err := h.cfg.Meter.Record(r.Context(), e)
	if err != nil {
		h.fail(w, r, "record", err)
		return
	}
	status := http.StatusOK
	if rc.Deferred {
		status = http.StatusAccepted // journaled: it succeeded, and has not been applied yet
	}
	writeJSON(w, status, receiptToJSON(rc))
}

func (h *handler) grant(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req grantRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "grant", err)
		return
	}
	if req.Scope == nil {
		h.fail(w, r, "grant", required("scope"))
		return
	}
	key, err := idemKey(r, req.IdemKey)
	if err != nil {
		h.fail(w, r, "grant", err)
		return
	}
	g := domain.Grant{
		Pool: domain.Pool(req.Pool), Amount: domain.Credits(req.Amount), IdemKey: key,
		Reason: req.Reason, SourceKey: req.SourceKey, Ref: mapOrNil(req.Ref),
	}
	if req.ExpiresAt != "" {
		at, err := parseTime(req.ExpiresAt, "expires_at")
		if err != nil {
			h.fail(w, r, "grant", err)
			return
		}
		g.ExpiresAt = &at
	}
	if g.Scope, err = p.Resolve(req.Scope.domain()); err != nil {
		h.fail(w, r, "grant", err)
		return
	}
	rc, err := h.cfg.Meter.Grant(r.Context(), g)
	if err != nil {
		h.fail(w, r, "grant", err)
		return
	}
	writeJSON(w, http.StatusOK, receiptToJSON(rc))
}

func (h *handler) transfer(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	var req transferRequest
	if err := decodeBody(r, &req); err != nil {
		h.fail(w, r, "transfer", err)
		return
	}
	switch {
	case req.From == nil:
		h.fail(w, r, "transfer", required("from"))
		return
	case req.To == nil:
		h.fail(w, r, "transfer", required("to"))
		return
	}
	key, err := idemKey(r, req.IdemKey)
	if err != nil {
		h.fail(w, r, "transfer", err)
		return
	}
	t := domain.Transfer{
		FromPool: domain.Pool(req.FromPool), ToPool: domain.Pool(req.ToPool), Amount: domain.Credits(req.Amount), IdemKey: key,
		Reason: req.Reason, MaxDestBalance: domain.Credits(req.MaxDestBalance), Ref: mapOrNil(req.Ref),
	}
	if t.From, err = p.Resolve(req.From.domain()); err != nil {
		h.fail(w, r, "transfer", err)
		return
	}
	if t.To, err = p.Resolve(req.To.domain()); err != nil {
		h.fail(w, r, "transfer", err)
		return
	}
	rc, err := h.cfg.Meter.Transfer(r.Context(), t)
	if err != nil {
		h.fail(w, r, "transfer", err)
		return
	}
	writeJSON(w, http.StatusOK, transferJSON{IdemKey: rc.IdemKey, Applied: rc.Applied, Amount: int64(rc.Amount), FromBalance: int64(rc.FromBalance), Status: rc.Status})
}

func (h *handler) balance(w http.ResponseWriter, r *http.Request) {
	p, ok := h.caller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	switch {
	case q.Get("scope_kind") == "":
		h.fail(w, r, "balance", required("scope_kind"))
		return
	case q.Get("scope_id") == "":
		h.fail(w, r, "balance", required("scope_id"))
		return
	}
	scope, err := p.Resolve(domain.Scope{Realm: domain.Realm(q.Get("scope_realm")), Kind: q.Get("scope_kind"), ID: q.Get("scope_id")})
	if err != nil {
		h.fail(w, r, "balance", err)
		return
	}
	pools, err := h.cfg.Balance.Balance(r.Context(), scope)
	if err != nil {
		h.fail(w, r, "balance", err)
		return
	}
	out := balanceJSON{Pools: make(map[string]int64, len(pools))}
	var total domain.Credits
	for pool, v := range pools {
		out.Pools[string(pool)] = int64(v)
		// Money is never allowed to wrap: a total that would is the server's fault, and an error.
		if total, err = total.Add(v); err != nil {
			h.fail(w, r, "balance", fmt.Errorf("balance total: %v", err)) //nolint:errorlint // deliberately not wrapped: ErrAmountOutOfRange would blame the caller
			return
		}
	}
	out.Total = int64(total)
	writeJSON(w, http.StatusOK, out)
}

// idemKey is the operation's idem_key: the Idempotency-Key header, which is required, which says it once, and
// which a body idem_key — if the body carries one — must agree with. A server-generated key would make every
// retry a new charge, so there is no default.
func idemKey(r *http.Request, bodyKey string) (string, error) {
	vals := r.Header.Values("Idempotency-Key")
	if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
		return "", fmt.Errorf("%w: the Idempotency-Key header is required: it is this operation's identity, and a retry carries the same one", domain.ErrInvalid)
	}
	if len(vals) > 1 {
		return "", fmt.Errorf("%w: send exactly one Idempotency-Key header", domain.ErrInvalid)
	}
	key := strings.TrimSpace(vals[0])
	if bodyKey != "" && bodyKey != key {
		return "", fmt.Errorf("%w: the body's idem_key differs from the Idempotency-Key header: the header is the key", domain.ErrInvalid)
	}
	return key, nil
}

func required(field string) error { return fmt.Errorf("%w: %s is required", domain.ErrInvalid, field) }

// fail answers an error with the status and code of the shared condition table. The caller's own
// cancellation wins over whatever the engine made of it; an UNAVAILABLE or INTERNAL condition answers with
// fixed text and the whole error goes to the operator's log.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, op string, err error) {
	if r.Context().Err() != nil {
		err = r.Context().Err()
	}
	var tooLarge bodyTooLarge
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", tooLarge.Error())
		return
	}
	c, ok := wire.Classify(err)
	if !ok {
		c = wire.Internal
	}
	msg := "internal error"
	switch {
	case !ok:
	case c.Verbatim:
		msg = err.Error()
	default:
		msg = c.Err.Error()
	}
	if c.HTTP >= 500 && c.HTTP != 504 {
		h.cfg.Logger.Log(r.Context(), slog.LevelError, "metering call failed", "op", op, "status", c.HTTP, "error", err)
	}
	writeError(w, c.HTTP, c.HTTPCode, msg)
}
