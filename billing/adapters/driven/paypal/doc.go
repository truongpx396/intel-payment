// Package paypal is the PayPal adapter behind ports.PaymentProvider. It is the only package that knows PayPal's wire
// format: how a webhook is verified, what its events look like, which calls create an order or a subscription. It speaks
// to PayPal's REST API directly — a few JSON calls — so no SDK sits in the dependency tree of a service that moves money.
//
// What it promises, and the tests hold it to:
//
//   - a webhook is verified by PayPal, which signs with a certificate this package never sees: the delivery's headers,
//     the account's webhook id and the event are sent to PayPal's verify call, and nothing is parsed before PayPal says
//     SUCCESS. The event is sent to PayPal byte for byte, and only if it is exactly one JSON value — anything else could
//     carry fields of its own into the request. What can be refused without PayPal (a header missing, a time outside the
//     tolerance, measured from when the delivery arrived) is refused without a call. When PayPal cannot answer the
//     delivery is domain.ErrVerificationUnavailable, so the ingress parks it with its raw body and answers 202: it is
//     neither rejected nor trusted;
//   - a verified event is normalized into the engine's vocabulary. PayPal has no customer object and a capture carries no
//     product, so an order or subscription is made with a custom_id of `<customer>~<product>`, and what comes back
//     without one is not ours and is ignored. A payment is keyed on the capture (or the sale of a subscription's cycle);
//     a refund is the total refunded to date; every phase of a dispute shares one id per transaction;
//   - what PayPal cannot do for a server it says so with domain.ErrUnsupported and does not call PayPal: saving a payment
//     method for later, charging one, a customer portal, cancelling at the period's end (PayPal cancels at once), and
//     changing a plan (PayPal's revision needs the buyer's approval, which the port has no way to ask for);
//   - an access token is fetched when needed, kept until a minute before it expires, replaced once if PayPal withdraws
//     it, and never appears in an error — nor does the client secret.
//
// An account's SecretRef resolves to `client_id:client_secret`, and its WebhookSecretRef to the id of the webhook as
// configured in PayPal's developer portal, which PayPal needs to verify a delivery. The provider price id the engine
// stores is a billing plan id for a subscription, and for a one-time purchase whatever the catalogue calls it: it is only
// carried in custom_id and read back.
//
// Known limits, for the host to know: PayPal reports a refund of a subscription's payment (PAYMENT.SALE.REFUNDED) with no
// cumulative total, so it is ignored and an operator reverses those credits; a PayPal account's other sales and disputes
// are not told apart from ours when a dispute names a transaction.
package paypal
