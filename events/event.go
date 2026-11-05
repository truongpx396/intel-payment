package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// APIVersion pins the shape of an envelope's data (outbound-events.md). A breaking change ships as a new version, and an endpoint keeps
// receiving the version it registered with until it opts in.
const APIVersion = "2026-09-01"

// The event types a deployment emits today. The catalogue in outbound-events.md is larger: a type with no producer yet is listed there as
// designed, and appears here when something can publish it.
const (
	CreditsLow          = "credits.low"
	CreditsWarning      = "credits.warning"
	CreditsBlocked      = "credits.blocked"
	PaymentSucceeded    = "payment.succeeded"
	PaymentFailed       = "payment.failed"
	PaymentRefunded     = "payment.refunded"
	PaymentDisputed     = "payment.disputed"
	PaymentChargedBack  = "payment.charged_back"
	EntitlementsChanged = "entitlements.changed"
)

// Types is every type a deployment emits, in catalogue order.
var Types = []string{
	CreditsLow, CreditsWarning, CreditsBlocked, PaymentSucceeded, PaymentFailed, PaymentRefunded, PaymentDisputed, PaymentChargedBack,
	EntitlementsChanged,
}

// Errors the stores and the use cases share. A transport maps them; none is ever shown to a caller with its cause.
var (
	ErrNotFound = errors.New("events: not found")
	ErrInvalid  = errors.New("events: invalid request")
)

// Event is one thing that happened, as the log holds it. The log is append-only: nothing about an event changes after it is written.
type Event struct {
	// ID is a UUID v7 the log issues under a lock, so ids sort in the order events became visible and a cursor never skips one.
	ID        string
	Realm     string
	Type      string
	ScopeKind string // empty for an event about no scope
	ScopeID   string
	// Data is the envelope's data object, as JSON. It carries scope ids and amounts, never card data, a provider secret or an email.
	Data      json.RawMessage
	CreatedAt time.Time
}

// ScopeRef names the scope an event is about.
type ScopeRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type envelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	APIVersion string          `json:"api_version"`
	CreatedAt  string          `json:"created_at"`
	Realm      string          `json:"realm"`
	Scope      *ScopeRef       `json:"scope,omitempty"`
	Data       json.RawMessage `json:"data"`
}

// Body is the envelope a host receives, as bytes. It is a pure function of the event, so the same event delivered twice, to two
// endpoints or by a feed and a webhook, is the same bytes with the same id: the host's dedup key.
func (e Event) Body() ([]byte, error) {
	if e.ID == "" || e.Type == "" || e.Realm == "" {
		return nil, fmt.Errorf("%w: an event needs an id, a type and a realm", ErrInvalid)
	}
	env := envelope{
		ID: e.ID, Type: e.Type, APIVersion: APIVersion, CreatedAt: e.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Realm: e.Realm, Data: e.Data,
	}
	if len(env.Data) == 0 {
		env.Data = json.RawMessage(`{}`)
	}
	if e.ScopeKind != "" || e.ScopeID != "" {
		env.Scope = &ScopeRef{Kind: e.ScopeKind, ID: e.ScopeID}
	}
	return json.Marshal(env)
}

// Endpoint is a place an event is sent. It is realm configuration (event_endpoints), written through the database and audited by it.
type Endpoint struct {
	ID    string
	Realm string
	URL   string
	// SecretRef names the HMAC signing secret (`env:NAME`, `file:/path`). During a rotation it names more than one, separated by commas,
	// and every request carries one signature per secret so the host can roll its side without a gap.
	SecretRef string
	// Types are the event types it subscribes to. An endpoint receives only these, and only its own realm's events.
	Types  []string
	Active bool
}

// Subscribes reports whether the endpoint wants an event of this type from this realm.
func (e Endpoint) Subscribes(realm, typ string) bool {
	if !e.Active || e.Realm != realm {
		return false
	}
	for _, t := range e.Types {
		if t == typ {
			return true
		}
	}
	return false
}

// Delivery states.
const (
	Pending   = "pending"
	Delivered = "delivered"
	Dead      = "dead"
)

// Due is a delivery that is owed: the event, where it goes, and how many times it has been tried.
type Due struct {
	DeliveryID string
	Event      Event
	Endpoint   Endpoint
	Attempts   int // failed attempts so far
	// CreatedAt is when the delivery was owed: the 72-hour retry window runs from it.
	CreatedAt time.Time
}
