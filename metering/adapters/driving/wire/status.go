package wire

import (
	"context"
	"fmt"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/truongpx396/intel-payment/api/meteringv1"
	"github.com/truongpx396/intel-payment/metering/domain"
)

// Status is the gRPC status of a failed call: the code of its row, an ErrorInfo whose reason lets a
// client recover the typed error, and a message that is the whole error where the caller can act on it
// and the sentinel's own text where the chain would name the topology. An error no row names is
// Internal with fixed text.
func Status(err error) *status.Status {
	c, ok := Classify(err)
	if !ok {
		return build(Internal, "internal error", nil)
	}
	msg := c.Err.Error()
	if c.Verbatim {
		msg = err.Error()
	}
	return build(c, msg, nil)
}

// DenialStatus is the status of a refused Admit: the code of its row and the whole Admission as a detail,
// beside the reason, so a client can return exactly what the in-process Meter returns. ok is false when the
// refusal names no ceiling, and the caller reports Internal.
func DenialStatus(adm domain.Admission) (*status.Status, bool) {
	c, ok := Denial(adm)
	if !ok {
		return nil, false
	}
	pb := AdmissionToProto(adm)
	var meta map[string]string
	if adm.Exceeded != nil && adm.Exceeded.Name != "" {
		meta = map[string]string{"limit": adm.Exceeded.Name}
	}
	msg := "the account is held by an operator"
	if !adm.Blocked {
		msg = fmt.Sprintf("limit %q is exhausted", adm.Exceeded.Name)
	}
	return build(c, msg, meta, pb), true
}

func build(c Condition, msg string, meta map[string]string, extra ...*meteringv1.Admission) *status.Status {
	st := status.New(c.GRPC, msg)
	// A detail that cannot be attached leaves a status with the right code, which is the part a caller
	// cannot do without.
	if with, err := st.WithDetails(&errdetails.ErrorInfo{Reason: c.Reason, Domain: ErrorDomain, Metadata: meta}); err == nil {
		st = with
	}
	for _, a := range extra {
		if with, err := st.WithDetails(a); err == nil {
			st = with
		}
	}
	return st
}

// Error is a typed error that crossed the wire. errors.Is(err, domain.ErrIdemConflict) holds on it by
// Unwrap, not by comparing text, and its Error() is the message the server wrote.
type Error struct {
	Condition Condition
	msg       string
}

func (e *Error) Error() string { return e.msg }

// Unwrap is the sentinel of the condition: what a caller tests with errors.Is. nil for a decision.
func (e *Error) Unwrap() error { return e.Condition.Err }

// reasonOf is the ErrorInfo.reason of a status this service wrote, "" for any other.
func reasonOf(st *status.Status) (string, map[string]string) {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == ErrorDomain {
			return info.GetReason(), info.GetMetadata()
		}
	}
	return "", nil
}

// Decode is the inverse of Status: the typed error a caller's code can errors.Is. A status that is not
// ours is returned untouched — a dead connection, a proxy's refusal, a newer server's reason are not
// guessed into a domain error — except the caller's own cancellation and deadline, which are the
// context's errors whoever noticed them.
func Decode(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	reason, _ := reasonOf(st)
	if c, ok := ByReason(reason); ok && reason != "" {
		return &Error{Condition: c, msg: st.Message()}
	}
	switch st.Code() {
	case codes.Canceled:
		return fmt.Errorf("%s: %w", st.Message(), context.Canceled)
	case codes.DeadlineExceeded:
		return fmt.Errorf("%s: %w", st.Message(), context.DeadlineExceeded)
	}
	return err
}

// DecodeDenial reports whether err is a refused Admit, and returns the Admission that refused it. The
// server carries the whole Admission; for one that does not (an older server), the reason and the limit
// name rebuild what the status says.
func DecodeDenial(err error) (domain.Admission, bool) {
	st, ok := status.FromError(err)
	if !ok {
		return domain.Admission{}, false
	}
	reason, meta := reasonOf(st)
	switch reason {
	case domain.DenyPaymentRequired, domain.DenyLimitReached, domain.DenyAccountBlocked:
	default:
		return domain.Admission{}, false
	}
	for _, d := range st.Details() {
		if a, ok := d.(*meteringv1.Admission); ok {
			if adm, err := AdmissionFromProto(a); err == nil {
				adm.Allowed = false
				return adm, true
			}
		}
	}
	adm := domain.Admission{Blocked: reason == domain.DenyAccountBlocked, Exceeded: &domain.Limit{Name: meta["limit"], DenyCode: reason}}
	return adm, true
}
