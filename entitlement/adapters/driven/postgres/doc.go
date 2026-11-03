// Package postgres is the entitlement store over PostgreSQL: a ports.EntitlementStore reading entitlement_keys,
// plan_entitlements, realm_defaults, entitlement_overrides and the subscriptions the payment layer keeps.
//
// It reads the payment layer's tables and imports none of its code: the layers share a schema, not a package. An override is
// written in one transaction that first tells the database who is acting and why, so the audit trigger (migrations/0004)
// records both with the row — and records them for a write that never came through here as well, with the database user as
// the actor (invariant 5).
package postgres
