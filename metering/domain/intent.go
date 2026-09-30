package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IntentOp is what a hot-tier mutation was.
type IntentOp string

const (
	OpUsage       IntentOp = "usage"
	OpGrant       IntentOp = "grant"
	OpTransferOut IntentOp = "transfer_out"
	OpTransferIn  IntentOp = "transfer_in"
	OpExpiry      IntentOp = "expiry"
	OpCorrection  IntentOp = "correction"
)

// IntentVersion is the schema version of an intent's stream fields.
const IntentVersion = 1

// PoolDelta is one pool's signed change.
type PoolDelta struct {
	Pool  Pool
	Delta Credits
}

// Intent is the record of one hot-tier mutation: the only thing the durable writer ever reads
// (hot-path-consistency.md §2). It is written by the hot functions, atomically with the balance
// change, and never by anything else.
type Intent struct {
	Op          IntentOp
	Scope       Scope
	Gen, Seq    int64  // the scope's generation and sequence number AS ISSUED
	IdemKey     string // the operation's key
	Fingerprint string
	// Draws are the pool deltas the hot tier actually applied, in draw order. For a usage intent
	// they are all ≤ 0; for a grant, one entry (which may be a clamped delta).
	Draws []PoolDelta
	// Writeoff is a clamp_to_zero / block_and_flag shortfall on a negative grant, booked as its own row.
	Writeoff Credits
	// LowWater is a balance watch ("<pool>:<threshold>") this mutation crossed downward.
	LowWater string
	Payload  Payload
}

// Payload is the app's JSON on an intent: everything the writer books beyond the pool deltas. It
// is built by the Meter and the writer, opaque to the hot functions, and versioned with the intent.
type Payload struct {
	Resource        string            `json:"resource,omitempty"`
	RateKey         string            `json:"rate_key,omitempty"`
	RateCardVersion string            `json:"rate_card,omitempty"`
	Quantities      []PayloadQuantity `json:"q,omitempty"`
	Subjects        map[string]string `json:"subjects,omitempty"`
	OccurredAt      string            `json:"occurred_at,omitempty"` // RFC 3339, UTC
	CostMicros      int64             `json:"cost_micros,omitempty"`
	Reason          string            `json:"reason,omitempty"`
	Ref             map[string]string `json:"ref,omitempty"`
	Actor           string            `json:"actor,omitempty"`
	TraceID         string            `json:"trace_id,omitempty"`

	// Grants and lots.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Transfers: the counterparty and pools. On transfer_out it names the destination; on
	// transfer_in, the source.
	Counterparty *PayloadScope `json:"counterparty,omitempty"`
	FromPool     string        `json:"from_pool,omitempty"`
	ToPool       string        `json:"to_pool,omitempty"`
	Amount       int64         `json:"amount,omitempty"`
	// Expiry and corrections.
	LotID      string `json:"lot_id,omitempty"`
	SuspenseID string `json:"suspense_id,omitempty"`
}

// PayloadQuantity is a Quantity in an intent's JSON.
type PayloadQuantity struct {
	Unit   string `json:"u"`
	Amount int64  `json:"n"`
}

// PayloadScope is a Scope in an intent's JSON.
type PayloadScope struct {
	Realm string `json:"realm"`
	Kind  string `json:"kind"`
	ID    string `json:"id"`
}

// PayloadScopeOf converts a Scope.
func PayloadScopeOf(s Scope) *PayloadScope {
	s = s.Normalized()
	return &PayloadScope{Realm: string(s.Realm), Kind: s.Kind, ID: s.ID}
}

// Scope converts back.
func (p PayloadScope) Scope() Scope { return Scope{Realm: Realm(p.Realm), Kind: p.Kind, ID: p.ID} }

// UsagePayload builds the payload of a usage intent from a Charge.
func UsagePayload(c Charge) Payload {
	p := Payload{
		Resource: c.Resource, RateKey: c.RateKey, RateCardVersion: c.RateCardVersion,
		CostMicros: c.CostMicros, Reason: c.Reason, Ref: c.Ref,
	}
	if len(c.Subjects) > 0 {
		p.Subjects = c.Subjects
	}
	for _, q := range c.Quantities {
		p.Quantities = append(p.Quantities, PayloadQuantity{Unit: string(q.Unit), Amount: q.Amount})
	}
	if !c.OccurredAt.IsZero() {
		p.OccurredAt = c.OccurredAt.UTC().Format(time.RFC3339Nano)
	}
	return p
}

// QuantitiesOf converts the payload's quantities back.
func (p Payload) QuantitiesOf() []Quantity {
	out := make([]Quantity, 0, len(p.Quantities))
	for _, q := range p.Quantities {
		out = append(out, Quantity{Unit: Unit(q.Unit), Amount: q.Amount})
	}
	return out
}

// Occurred parses OccurredAt; the zero time when absent.
func (p Payload) Occurred() (time.Time, error) {
	if p.OccurredAt == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, p.OccurredAt)
}

// JSON encodes the payload for the hot function's ARGV.
func (p Payload) JSON() (string, error) {
	b, err := json.Marshal(p)
	return string(b), err
}

// FormatDraws renders pool deltas as the hot functions write them: "general:-11000;promo:-500".
func FormatDraws(ds []PoolDelta) string {
	parts := make([]string, len(ds))
	for i, d := range ds {
		parts[i] = string(d.Pool) + ":" + strconv.FormatInt(int64(d.Delta), 10)
	}
	return strings.Join(parts, ";")
}

// ParseDraws is the inverse of FormatDraws. An empty string is no draws (a zero-amount usage).
func ParseDraws(s string) ([]PoolDelta, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ";")
	out := make([]PoolDelta, 0, len(parts))
	for _, part := range parts {
		i := strings.LastIndexByte(part, ':')
		if i <= 0 {
			return nil, fmt.Errorf("%w: malformed draw %q", ErrInvalid, part)
		}
		n, err := strconv.ParseInt(part[i+1:], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: malformed draw %q", ErrInvalid, part)
		}
		out = append(out, PoolDelta{Pool: Pool(part[:i]), Delta: Credits(n)})
	}
	return out, nil
}

// Total is the sum of the draws.
func (i Intent) Total() (Credits, error) {
	var t Credits
	for _, d := range i.Draws {
		var err error
		if t, err = t.Add(d.Delta); err != nil {
			return 0, err
		}
	}
	return t, nil
}

// Fields renders the intent as the stream fields the hot functions write. It exists for tests and
// fakes: production intents are written by the Lua functions, never by Go.
func (i Intent) Fields() (map[string]string, error) {
	p, err := i.Payload.JSON()
	if err != nil {
		return nil, err
	}
	s := i.Scope.Normalized()
	f := map[string]string{
		"v": strconv.Itoa(IntentVersion), "op": string(i.Op),
		"realm": string(s.Realm), "kind": s.Kind, "id": s.ID,
		"gen": strconv.FormatInt(i.Gen, 10), "seq": strconv.FormatInt(i.Seq, 10),
		"idem": i.IdemKey, "fp": i.Fingerprint, "draws": FormatDraws(i.Draws), "p": p,
	}
	if i.Writeoff != 0 {
		f["wo"] = strconv.FormatInt(int64(i.Writeoff), 10)
	}
	if i.LowWater != "" {
		f["lw"] = i.LowWater
	}
	return f, nil
}

// IntentFromFields parses the stream fields of an intent. An error means the entry is malformed;
// the writer parks it (FR-043) rather than dropping it.
func IntentFromFields(f map[string]string) (Intent, error) {
	var in Intent
	if v := f["v"]; v != strconv.Itoa(IntentVersion) {
		return in, fmt.Errorf("%w: unsupported intent version %q", ErrInvalid, v)
	}
	in.Op = IntentOp(f["op"])
	switch in.Op {
	case OpUsage, OpGrant, OpTransferOut, OpTransferIn, OpExpiry, OpCorrection:
	default:
		return in, fmt.Errorf("%w: unknown intent op %q", ErrInvalid, f["op"])
	}
	in.Scope = Scope{Realm: Realm(f["realm"]), Kind: f["kind"], ID: f["id"]}
	var err error
	if in.Gen, err = strconv.ParseInt(f["gen"], 10, 64); err != nil {
		return in, fmt.Errorf("%w: intent gen %q", ErrInvalid, f["gen"])
	}
	if in.Seq, err = strconv.ParseInt(f["seq"], 10, 64); err != nil || in.Seq < 1 {
		return in, fmt.Errorf("%w: intent seq %q", ErrInvalid, f["seq"])
	}
	in.IdemKey, in.Fingerprint, in.LowWater = f["idem"], f["fp"], f["lw"]
	if in.IdemKey == "" || in.Fingerprint == "" {
		return in, fmt.Errorf("%w: intent without idem or fp", ErrInvalid)
	}
	if in.Draws, err = ParseDraws(f["draws"]); err != nil {
		return in, err
	}
	if wo := f["wo"]; wo != "" {
		n, err := strconv.ParseInt(wo, 10, 64)
		if err != nil {
			return in, fmt.Errorf("%w: intent wo %q", ErrInvalid, wo)
		}
		in.Writeoff = Credits(n)
	}
	if p := f["p"]; p != "" {
		if err := json.Unmarshal([]byte(p), &in.Payload); err != nil {
			return in, fmt.Errorf("%w: intent payload: %w", ErrInvalid, err)
		}
	}
	return in, in.Scope.Validate()
}
