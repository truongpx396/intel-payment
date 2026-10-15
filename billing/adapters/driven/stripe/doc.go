// Package stripe is the Stripe adapter behind ports.PaymentProvider. It is the only package that knows Stripe's wire
// format: how a webhook is signed, what its events look like, which calls create a customer, a checkout or a
// schedule. It speaks to Stripe's HTTP API directly — a few form-encoded calls — so no SDK sits in the dependency
// tree of a service that moves money, and every byte it sends is one this package wrote.
//
// What it promises, and the tests hold it to:
//
//   - a webhook is verified on the raw body, in constant time (hmac.Equal and nothing else), within a timestamp
//     tolerance, with the secret of the account the route names — before anything is parsed;
//   - a verified event is normalized into the engine's vocabulary: a payment is keyed on the PAYMENT INTENT, so an
//     invoice and any other event that names the payment are one grant; a refund is the cumulative total; a checkout
//     session is ignored, because a checkout return never fulfils;
//   - a secret is read from its reference at the moment of use and never appears in an error.
//
// The events and API version are those of 2024-06-20; a webhook endpoint must be pinned to it.
package stripe
