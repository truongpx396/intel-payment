// Package paymenthttp is the payment half of the /v1 REST surface: the routes a host's backend calls to sell, to show
// what was sold, and to manage a customer's subscription and standing top-up (rest-api.md, "Catalogue, checkout &
// subscription").
//
// It speaks HTTP for billing's ports and nothing more. It reaches the use cases only through billing/ports, holds no
// secret, calls no provider, and has no Meter — it cannot grant a credit, which is the point: credits arrive when a
// verified payment is processed (payments invariant 2). What a thing costs is never read from a request (invariant 4):
// a checkout names a plan and a currency, and the catalogue says the rest.
//
// The routes are mounted by the REST handler as /v1 routes, so the registry authenticates every call and checks the
// route's privilege before a handler here runs. This package decides which privilege a route needs and, per call, which
// realm the credential means (Caller): the realm is the credential's and never the body's. It imports no transport — the
// credential lives with the REST handler — which is why Caller is an interface the binary fills.
package paymenthttp
