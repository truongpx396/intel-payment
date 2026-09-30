// Package domain holds the engine's pure types and the rules that need no infrastructure: scopes and
// their placement, credits and money, events and prices, rate cards, limits and their evaluation,
// receipts, the canonical fingerprints that make idempotency exact, and the typed errors a caller
// acts on.
//
// It imports nothing outside the standard library, and reads no clock: the wall clock and every
// store arrive through ports, so everything here is replayable (constitution VI).
package domain
