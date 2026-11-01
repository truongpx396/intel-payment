# Sandbox rehearsal

Before a real card is charged, each provider adapter has to meet the provider. Until October 2026 none had: they were
written from documentation and tried against fixtures built from the same documentation, which can only confirm what the
author already believed. This page records the rehearsal that ended that, what it found, what it could not reach, and how to
repeat it for your own account.

The rehearsal was driven by throwaway scripts that are **not** in the repository; what is kept is the evidence — the captured
deliveries the adapters are now tested against — and this procedure. Nothing here replaces redoing it for the account, the
currency and the products you will actually sell.

## What was run

The whole stack (`paymentd`, `payment-worker`, PostgreSQL, Redis) ran locally, reachable by the providers through a tunnel
(`cloudflared tunnel --url http://localhost:8080`). Provider accounts, plans, the provider's prices and customers were
created **through the admin API** (`/v1/admin/…`), a host's calls through the `/v1` routes, and every webhook the providers
sent was received by the real `POST /webhooks/{provider}/{account}` door and fulfilled by the real worker.

| Provider | Run against | Exercised |
|---|---|---|
| Stripe (API 2024-06-20) | a test-mode account | one-time pack, subscription (create, plan change, cancel), hosted checkout in `payment`, `subscription` and `setup` modes, billing portal, a failed payment, partial refunds, a dispute lost and then won, an auto top-up charging a saved card off-session |
| Polar | the sandbox | webhook signature, a free order's `order.created` / `order.paid`, product and checkout creation, the grant |
| PayPal | the sandbox, and its webhook simulator | OAuth, order creation and its approval link, **PayPal's own verify call** on simulator deliveries, the dispute events |

117 deliveries were captured; the useful ones are kept, scrubbed (identities, links and keys removed; the sandbox's price ids
replaced by the contract world's) under `billing/contracts/{stripe,polar,paypal}fix/captured/`, and each adapter's
`captured_test.go` holds it to them.

## What it found

Two defects, both fixed test-first (a `test(red)` commit that failed, then the fix):

1. **An auto top-up charged the card and granted nothing.** Stripe's `payment_intent.succeeded` for an off-session charge has
   an amount and a method and no price (a payment intent has none), and the processor resolved the plan from the price, so it
   ignored the event as "a price this deployment does not sell". The processor now takes the plan from the pending payment the
   auto top-up handler recorded when it charged — never from the event — and a charge whose plan is no longer sold is a loud
   error that pages a person, not an ignore. (`billing/app/processor.go`, `planOf`.)
2. **A first checkout on a new Stripe account was a bare 500.** New accounts have *Managed Payments* on, and a Checkout Session
   created at API 2024-06-20 is then refused with a 400 that carries no code and no param, only a message naming it. The
   adapter retries once with `managed_payments[enabled]=false` under its own idempotency key; any other 400 is an error as
   before. (`billing/adapters/driven/stripe/api.go`.)

And three things that were configuration, not code, worth knowing before you start:

- **`make up` ignored the repository's `.env`.** Compose reads `.env` from the compose file's directory, so no credential
  reached the containers. The Makefile now passes `--env-file .env` when it exists.
- **A Stripe account's default currency is its country's** (SGD for a Singapore account). A price or invoice in another currency
  must say so, or it is made in the wrong one — and a zero-amount invoice is paid at once and proves nothing.
- **Polar refuses reserved email domains** such as `example.com` for a customer.

## What it could not reach

State this rather than let "rehearsed" be read as "covered":

- **PayPal with a buyer.** A capture needs a person to approve the order in a browser. The simulator is not a buyer: it sends some
  samples in the legacy v1 shape (its `PAYMENT.CAPTURE.REFUNDED` is a sale's refund with no capture and no total refunded, which
  the adapter refuses loudly — the delivery is retried and alerts — rather than reading), and its disputes name transactions this
  service never recorded, so they are retried until dead. So PayPal's v2 capture, refund and reversal shapes, whether
  `total_refunded_amount` is on every refund, and whether a wallet order is captured on approval remain **unrehearsed**.
- **Polar with money.** The captured order is a free one; a paid order, a refund and a renewal were not captured.
- **PayPal subscriptions** and **Stripe test clocks** (dunning and the grace window over real time).
- **A live-mode account.** Its settings (Managed Payments, tax, payment-method configuration) can differ from a sandbox's.
- **Automatic capture of a saved payment method from a `setup` checkout.** The adapters can create one; nothing yet reads the
  resulting method into the auto top-up instruction, so the host passes `payment_method_ref` itself.

## Repeating it

1. Test-mode credentials only. Put them in `.env` (see [configuration.md](configuration.md#provider-secrets)): `STRIPE_SECRET_KEY`,
   `POLAR_ACCESS_TOKEN`, and `PAYPAL_CREDENTIALS` as **`<client id>:<client secret>` in one value**. A wrong half gets a 401 only
   when PayPal is first called, so check it before relying on it.
2. `make up`, and expose `localhost:8080` through a tunnel. Keep the tunnel's URL: a quick tunnel changes it on restart, and every
   webhook endpoint below must be registered again.
3. Give a service token the `admin` privilege (and `grant` for the subscription routes) in `PAYMENT_SERVICE_TOKENS`, then through `/v1/admin`:
   `PUT /v1/admin/provider-accounts/{id}` per provider with *references* to the secrets (`env:NAME`), `POST /v1/admin/plans`,
   `PUT …/provider-prices/{account}/{currency}` to map each plan to the provider's price, and `PUT /v1/admin/customers` to link a
   scope to the provider's customer.
4. Register `…/webhooks/{provider}/{account}` at each provider and put **that endpoint's own** signing secret in the variable the
   account references. A secret from a different endpoint verifies nothing, and the symptom is a `400` on every delivery.
   Stripe's endpoint must be pinned to API version `2024-06-20`.
5. Run each flow of [the integration guide's checklist](integration-guide.md#7-verify-before-you-trust-it) in the provider's sandbox,
   and read `payment_events` and the books after each, not just the HTTP status.
6. Delete the webhook endpoints, products and customers you created when you are done. They are in your account, not ours.

Use test keys only. The admin API refuses a raw secret where a reference belongs and a reference in a scheme the process does not
resolve, but it cannot know what a variable holds, so that check is yours.
