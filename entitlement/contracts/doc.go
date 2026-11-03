// Package contracts holds the conformance suites of the entitlement layer and the in-memory store they run on:
// EntitlerContract (every implementation of ports.Entitler) and StoreContract (every ports.EntitlementStore). A suite
// imports no adapter and no use case — it is given a constructor — and is held to the rule it enforces: shown to fail
// against a deliberately broken implementation, by name, or it has not been shown to test anything.
package contracts
