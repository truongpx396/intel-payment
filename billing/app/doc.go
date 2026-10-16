// Package app is the fiat layer's use cases: the webhook ingress (stage 1: verify, persist, acknowledge), the
// processor (stage 2: fulfil from the inbox), checkout, the tax and plan-change policies and auto top-up. It
// mints credits through metering's Meter port exactly as any other host would, and decides nothing about a
// provider's wire format.
package app
