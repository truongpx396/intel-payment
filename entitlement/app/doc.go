// Package app resolves what a scope is entitled to and answers a host's questions about it. Resolution is a pure function of
// what the store holds and the instant it is asked: the free tier, the subscription if it holds, the scope's overrides, merged
// within a level and ranked across levels. The Entitler is that function behind the ports a host calls.
//
// It imports no database, no cache and no transport; the store is a port, and the clock is injected so a grace window and an
// override's expiry can be crossed in a test without waiting for them.
package app
