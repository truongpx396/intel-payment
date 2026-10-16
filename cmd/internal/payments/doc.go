// Package payments connects the payment layer to the binaries under cmd/. It holds what only a process knows: where a
// provider account's secret lives (a reference that becomes a secret at the moment of use), which provider adapters the
// binaries serve, and the loops payment-worker runs for payments. The use cases are billing/app's and the adapters
// billing/adapters'; this package decides nothing about money.
package payments
