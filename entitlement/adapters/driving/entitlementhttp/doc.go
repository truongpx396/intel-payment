// Package entitlementhttp is the REST door of the entitlement layer: GET /v1/entitlements (every effective grant, with its
// provenance) and POST /v1/entitlements/check (a batch of questions, answered from one resolution). It speaks the wire contract
// of api/proto/entitlement/v1 as JSON — the same messages the gRPC door returns, rendered with 64-bit integers as numbers and
// not strings, because -1 on a quota is UNLIMITED and "-1" is not.
//
// The door reaches the use cases only through the entitlement ports and learns whose realm a call is for through a Caller, so it
// imports no transport; the binary mounts its routes on the REST handler's /v1 surface, which authenticates every call and checks
// the route's privilege before a handler runs.
package entitlementhttp
