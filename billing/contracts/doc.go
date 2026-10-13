// Package contracts holds the conformance suites of the fiat layer and the in-memory collaborators they run on:
// ProviderContract (every provider adapter), IngressContract (stage 1 of the webhook flow) and the processor's
// suites (stage 2). A suite imports no adapter — it is given constructors — and is held to the rule it enforces:
// shown to fail against a deliberately broken implementation, or it has not been shown to test anything.
package contracts
