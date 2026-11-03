// Package ports is the entitlement layer's seams: the Entitler a host asks, and the EntitlementStore the answers are
// resolved from. Everything between the two is evaluation, and evaluation is deliberately dull — a capability check that is
// clever is a capability check nobody can predict.
package ports
