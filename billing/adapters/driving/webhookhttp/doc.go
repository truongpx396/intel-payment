// Package webhookhttp is the door a payment provider knocks on: POST /webhooks/{provider}/{account}.
//
// It reads the raw body — size-limited, never parsed, never altered, because a signature covers those exact bytes — and
// hands it, the headers and the two names the route carries to the ingress, which verifies, persists and decides. The
// door speaks HTTP for the ingress and nothing more: it holds no secret, reads no store and learns nothing about a
// provider. It is mounted by the REST handler as a webhook route, which is why the route carries no authentication and
// no CORS (webhook invariant 11): the provider's signature is the credential, and the ingress is what checks it.
package webhookhttp
