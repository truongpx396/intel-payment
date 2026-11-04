// Package entitlements is what the binaries know about the entitlement layer that the layer itself must not: how to open it over
// a database and a bus, whose credential a call is, and how to mount its doors. paymentd serves the entitlements — REST and gRPC
// behind the credential, and the QuotaSource that sizes plan-dependent limits — and payment-worker runs the config relay that tells
// every replica's cache when configuration changes.
package entitlements
