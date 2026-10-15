// Package polar is the Polar adapter behind ports.PaymentProvider. It is the only package that knows Polar's wire
// format: how a webhook is signed, what its events look like, which calls create a customer, a checkout or a portal
// session. It speaks to Polar's HTTP API directly — a few JSON calls — so no SDK sits in the dependency tree of a
// service that moves money, and every byte it sends is one this package wrote.
//
// What it promises, and the tests hold it to:
//
//   - a webhook is verified on the raw body, in constant time (hmac.Equal and nothing else), within a timestamp
//     tolerance, with the secret of the account the route names — before anything is parsed. The signature covers the
//     message id and the timestamp as well as the body, so a captured delivery cannot be replayed as a new event or a
//     fresh one. A secret is accepted under both of Polar's keys (Standard Webhooks', and the older one made from the
//     whole whsec_ string), because the dashboard does not say which a secret is;
//   - a verified event is normalized into the engine's vocabulary: a payment is keyed on the ORDER, so order.created,
//     order.paid and order.updated are one grant; a refund is the cumulative total, the money and its sales tax;
//     checkouts, refund objects and benefit grants fulfil and revoke nothing;
//   - what Polar cannot do it says so with domain.ErrUnsupported and does not call Polar: saving a payment method for
//     later, charging one, and a plan change "now" without proration;
//   - a secret is read from its reference at the moment of use and never appears in an error.
//
// For Polar the provider price id the engine stores is the Polar PRODUCT id. Polar is the merchant of record: it
// collects and remits the tax and settles chargebacks itself, so it sends no dispute events and no failed-payment
// events (a failing renewal arrives as subscription.past_due). Two Polar limits are the host's to know: an email
// address is unique within a Polar organization, so two scopes cannot bill to one address on one account; and a
// checkout cannot be made idempotent, so a double-clicked buy button makes two checkout pages — which fulfil nothing
// until an order is paid.
package polar
