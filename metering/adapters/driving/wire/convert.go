package wire

import (
	"fmt"
	"math"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// Conversions between the wire messages and the domain, shared by the server and the client so the two
// halves of a round trip are written once. A conversion that cannot represent a value says so (ErrInvalid)
// and never rounds, defaults or truncates it: the wire is a pass-through, and a value it changes is a
// different request.

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", domain.ErrInvalid, fmt.Sprintf(format, a...))
}

// -------------------------------------------------------------------- time --

// TimeFromProto reads a timestamp. An absent one is the zero time — "not given" — and never the epoch:
// a missing occurred_at must not become a very stale event. field names it in the error.
func TimeFromProto(ts *timestamppb.Timestamp, field string) (time.Time, error) {
	if ts == nil {
		return time.Time{}, nil
	}
	if err := ts.CheckValid(); err != nil {
		return time.Time{}, invalid("%s is not a valid timestamp: %v", field, err)
	}
	return ts.AsTime(), nil
}

// TimeToProto is the inverse. The zero time is an absent timestamp.
func TimeToProto(t time.Time, field string) (*timestamppb.Timestamp, error) {
	if t.IsZero() {
		return nil, nil //nolint:nilnil // an absent timestamp is the answer, not a failure
	}
	ts := timestamppb.New(t.UTC())
	if err := ts.CheckValid(); err != nil {
		return nil, invalid("%s is not representable as a timestamp: %v", field, err)
	}
	return ts, nil
}

// ------------------------------------------------------------------- scope --

// ScopeFromProto reads a scope. The caller has checked it is present.
func ScopeFromProto(s *meteringv1.Scope) domain.Scope {
	return domain.Scope{Realm: domain.Realm(s.GetRealm()), Kind: s.GetKind(), ID: s.GetId()}
}

// ScopeToProto is the inverse.
func ScopeToProto(s domain.Scope) *meteringv1.Scope {
	return &meteringv1.Scope{Realm: string(s.Realm), Kind: s.Kind, Id: s.ID}
}

// ----------------------------------------------------------------- strings --

func subjectsFromProto(m map[string]string) domain.Subjects {
	if len(m) == 0 {
		return nil
	}
	out := make(domain.Subjects, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func stringsFromProto(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func stringsToProto(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ------------------------------------------------------------------ limits --

var windowToProto = map[domain.Window]meteringv1.Window{
	domain.Balance: meteringv1.Window_BALANCE, domain.Daily: meteringv1.Window_DAILY, domain.Hourly: meteringv1.Window_HOURLY,
	domain.Rolling: meteringv1.Window_ROLLING, domain.Job: meteringv1.Window_JOB,
}

// LimitFromProto reads a limit. field names it ("limits[2]") in an error.
func LimitFromProto(l *meteringv1.Limit, field string) (domain.Limit, error) {
	var w domain.Window
	found := false
	for dw, pw := range windowToProto {
		if pw == l.GetWindow() {
			w, found = dw, true
		}
	}
	if !found {
		return domain.Limit{}, invalid("%s: window %d is not a window this service knows", field, int32(l.GetWindow()))
	}
	secs := l.GetDurSeconds()
	if secs < 0 || secs > int64(math.MaxInt64/time.Second) {
		return domain.Limit{}, invalid("%s: dur_seconds %d is out of range", field, secs)
	}
	return domain.Limit{
		Name: l.GetName(), Subject: l.GetSubject(), Resource: l.GetResource(), Unit: domain.Unit(l.GetUnit()),
		Max: l.GetMax(), Window: w, Dur: time.Duration(secs) * time.Second, TZ: l.GetTz(), WarnAt: l.GetWarnAt(),
		DenyCode: l.GetDenyCode(), MaxEntitlement: l.GetMaxEntitlement(),
	}, nil
}

// LimitToProto is the inverse. A duration that is not a whole number of seconds cannot be said on the
// wire, and it is refused rather than truncated: a rolling window one second short is not the window the
// caller asked for.
func LimitToProto(l domain.Limit, field string) (*meteringv1.Limit, error) {
	w, ok := windowToProto[l.Window]
	if !ok {
		return nil, invalid("%s: window %d is not a window this service knows", field, int(l.Window))
	}
	if l.Dur < 0 || l.Dur%time.Second != 0 {
		return nil, invalid("%s: dur %s must be a whole, non-negative number of seconds on the wire", field, l.Dur)
	}
	return &meteringv1.Limit{
		Name: l.Name, Subject: l.Subject, Resource: l.Resource, Unit: string(l.Unit), Max: l.Max, Window: w,
		DurSeconds: int64(l.Dur / time.Second), Tz: l.TZ, WarnAt: l.WarnAt, DenyCode: l.DenyCode, MaxEntitlement: l.MaxEntitlement,
	}, nil
}

// ------------------------------------------------------------------- admit --

// AdmitRequestFromProto reads an admission request, without its scope (which the transport resolves
// against the credential first).
func AdmitRequestFromProto(r *meteringv1.AdmitRequest) (domain.AdmitRequest, error) {
	out := domain.AdmitRequest{Resource: r.GetResource(), Subjects: subjectsFromProto(r.GetSubjects()), MaxCost: domain.Credits(r.GetMaxCost())}
	for i, l := range r.GetLimits() {
		dl, err := LimitFromProto(l, fmt.Sprintf("limits[%d]", i))
		if err != nil {
			return domain.AdmitRequest{}, err
		}
		out.Limits = append(out.Limits, dl)
	}
	return out, nil
}

// AdmitRequestToProto is the inverse.
func AdmitRequestToProto(s domain.Scope, r domain.AdmitRequest) (*meteringv1.AdmitRequest, error) {
	out := &meteringv1.AdmitRequest{Scope: ScopeToProto(s), Resource: r.Resource, Subjects: stringsToProto(r.Subjects), MaxCost: int64(r.MaxCost)}
	for i, l := range r.Limits {
		pl, err := LimitToProto(l, fmt.Sprintf("limits[%d]", i))
		if err != nil {
			return nil, err
		}
		out.Limits = append(out.Limits, pl)
	}
	return out, nil
}

// AdmissionToProto converts an admission. A limit the wire cannot carry (a duration with a fraction of a
// second — which a configured limit, stored in whole seconds, never has) is sent without its duration
// rather than failing the answer.
func AdmissionToProto(a domain.Admission) *meteringv1.Admission {
	out := &meteringv1.Admission{Allowed: a.Allowed, Headroom: int64(a.Headroom), Blocked: a.Blocked}
	if a.Exceeded != nil {
		out.Exceeded = limitForAnswer(*a.Exceeded)
	}
	if a.Warning != nil {
		out.Warning = limitForAnswer(*a.Warning)
	}
	return out
}

func limitForAnswer(l domain.Limit) *meteringv1.Limit {
	if pl, err := LimitToProto(l, "limit"); err == nil {
		return pl
	}
	l.Dur = l.Dur.Truncate(time.Second)
	pl, err := LimitToProto(l, "limit")
	if err != nil {
		return &meteringv1.Limit{Name: l.Name, DenyCode: l.DenyCode}
	}
	return pl
}

// AdmissionFromProto reads an admission.
func AdmissionFromProto(a *meteringv1.Admission) (domain.Admission, error) {
	out := domain.Admission{Allowed: a.GetAllowed(), Headroom: domain.Credits(a.GetHeadroom()), Blocked: a.GetBlocked()}
	if a.GetExceeded() != nil {
		l, err := LimitFromProto(a.GetExceeded(), "exceeded")
		if err != nil {
			return domain.Admission{}, err
		}
		out.Exceeded = &l
	}
	if a.GetWarning() != nil {
		l, err := LimitFromProto(a.GetWarning(), "warning")
		if err != nil {
			return domain.Admission{}, err
		}
		out.Warning = &l
	}
	return out, nil
}

// ------------------------------------------------------------------ record --

// EventFromProto reads an event, without validating that its key and time are present: the transport
// refuses those first, by name.
func EventFromProto(e *meteringv1.Event) (domain.Event, error) {
	at, err := TimeFromProto(e.GetOccurredAt(), "occurred_at")
	if err != nil {
		return domain.Event{}, err
	}
	out := domain.Event{
		Scope: ScopeFromProto(e.GetScope()), Resource: e.GetResource(), RateKey: e.GetRateKey(),
		Subjects: subjectsFromProto(e.GetSubjects()), IdemKey: e.GetIdemKey(), OccurredAt: at, Attributes: stringsFromProto(e.GetAttributes()),
	}
	for _, q := range e.GetQuantities() {
		out.Quantities = append(out.Quantities, domain.Quantity{Unit: domain.Unit(q.GetUnit()), Amount: q.GetAmount()})
	}
	return out, nil
}

// EventToProto is the inverse.
func EventToProto(e domain.Event) (*meteringv1.Event, error) {
	at, err := TimeToProto(e.OccurredAt, "occurred_at")
	if err != nil {
		return nil, err
	}
	out := &meteringv1.Event{
		Scope: ScopeToProto(e.Scope), Resource: e.Resource, RateKey: e.RateKey, IdemKey: e.IdemKey, OccurredAt: at,
		Attributes: stringsToProto(e.Attributes), Subjects: stringsToProto(e.Subjects),
	}
	for _, q := range e.Quantities {
		out.Quantities = append(out.Quantities, &meteringv1.Quantity{Unit: string(q.Unit), Amount: q.Amount})
	}
	return out, nil
}

// ReceiptToProto and ReceiptFromProto carry every field of a receipt, the writeoff included.
func ReceiptToProto(r domain.Receipt) *meteringv1.Receipt {
	return &meteringv1.Receipt{
		IdemKey: r.IdemKey, Applied: r.Applied, Deferred: r.Deferred, Delta: int64(r.Delta), Balance: int64(r.Balance),
		Seq: r.Seq, RateCardVersion: r.RateCardVersion, Writeoff: int64(r.Writeoff),
	}
}

func ReceiptFromProto(r *meteringv1.Receipt) domain.Receipt {
	return domain.Receipt{
		IdemKey: r.GetIdemKey(), Applied: r.GetApplied(), Deferred: r.GetDeferred(), Delta: domain.Credits(r.GetDelta()),
		Balance: domain.Credits(r.GetBalance()), Seq: r.GetSeq(), RateCardVersion: r.GetRateCardVersion(), Writeoff: domain.Credits(r.GetWriteoff()),
	}
}

// ------------------------------------------------------------------- grant --

func GrantFromProto(g *meteringv1.GrantRequest) (domain.Grant, error) {
	exp, err := TimeFromProto(g.GetExpiresAt(), "expires_at")
	if err != nil {
		return domain.Grant{}, err
	}
	out := domain.Grant{
		Scope: ScopeFromProto(g.GetScope()), Pool: domain.Pool(g.GetPool()), Amount: domain.Credits(g.GetAmount()), IdemKey: g.GetIdemKey(),
		Reason: g.GetReason(), SourceKey: g.GetSourceKey(), Ref: stringsFromProto(g.GetRef()),
	}
	if !exp.IsZero() {
		out.ExpiresAt = &exp
	}
	return out, nil
}

func GrantToProto(g domain.Grant) (*meteringv1.GrantRequest, error) {
	var exp *timestamppb.Timestamp
	if g.ExpiresAt != nil {
		var err error
		if exp, err = TimeToProto(*g.ExpiresAt, "expires_at"); err != nil {
			return nil, err
		}
	}
	return &meteringv1.GrantRequest{
		Scope: ScopeToProto(g.Scope), Pool: string(g.Pool), Amount: int64(g.Amount), IdemKey: g.IdemKey, Reason: g.Reason,
		ExpiresAt: exp, Ref: stringsToProto(g.Ref), SourceKey: g.SourceKey,
	}, nil
}

// ---------------------------------------------------------------- transfer --

func TransferFromProto(t *meteringv1.TransferRequest) domain.Transfer {
	return domain.Transfer{
		From: ScopeFromProto(t.GetFrom()), To: ScopeFromProto(t.GetTo()), FromPool: domain.Pool(t.GetFromPool()), ToPool: domain.Pool(t.GetToPool()),
		Amount: domain.Credits(t.GetAmount()), IdemKey: t.GetIdemKey(), Reason: t.GetReason(),
		MaxDestBalance: domain.Credits(t.GetMaxDestBalance()), Ref: stringsFromProto(t.GetRef()),
	}
}

func TransferToProto(t domain.Transfer) *meteringv1.TransferRequest {
	return &meteringv1.TransferRequest{
		From: ScopeToProto(t.From), To: ScopeToProto(t.To), FromPool: string(t.FromPool), ToPool: string(t.ToPool),
		Amount: int64(t.Amount), IdemKey: t.IdemKey, Reason: t.Reason, MaxDestBalance: int64(t.MaxDestBalance), Ref: stringsToProto(t.Ref),
	}
}

func TransferReceiptToProto(r domain.TransferReceipt) *meteringv1.TransferReply {
	return &meteringv1.TransferReply{IdemKey: r.IdemKey, Applied: r.Applied, Amount: int64(r.Amount), FromBalance: int64(r.FromBalance), Status: r.Status}
}

func TransferReceiptFromProto(r *meteringv1.TransferReply) domain.TransferReceipt {
	return domain.TransferReceipt{
		IdemKey: r.GetIdemKey(), Applied: r.GetApplied(), Amount: domain.Credits(r.GetAmount()), FromBalance: domain.Credits(r.GetFromBalance()), Status: r.GetStatus(),
	}
}
