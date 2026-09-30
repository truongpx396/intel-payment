// Package contracts holds the conformance suites for the metering ports. A suite runs UNCHANGED
// against every implementation of its port, so "this seam is generic" is a test result rather than
// an assertion (constitution VII). It is an ordinary package, not a _test file, so that an
// adapter's own tests can import it and hand it a constructor.
//
// PricerContract is pure — no infrastructure. The suites that need Redis and Postgres
// (LedgerContract, LedgerWriterContract, ConsistencyContract) build on Testcontainers and live
// behind the `integration` build tag in the adapters that run them.
package contracts
