// Package domain is the entitlement layer's vocabulary and its pure rules: a capability's key and kind, the closed union a
// grant's value is, where a grant came from, and the arithmetic that decides whether a count fits a quota.
//
// It imports only metering's domain (for the scope and the engine's errors) and reads no clock and no store. The two
// things that must never go wrong live here, so they can be held to properties: -1 means UNLIMITED and is never compared as
// a number, coerced to a large one, or taken for "unset" (entitlement invariant 6); and a count is measured against a quota
// without int64 wrapping — a sum that cannot be represented is refused, never wrapped.
package domain
