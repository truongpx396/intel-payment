// Package postgres is the fiat layer's books on PostgreSQL: provider accounts, customers, the catalogue, the webhook
// inbox, payments, subscriptions and the auto top-up instruction. Every store is held to the same contract as its
// in-memory twin (billing/contracts), and the guarantees that matter are the database's, not a lock's: a unique
// constraint is the replay guard, a CHECK is what stops a refund past the payment, and an advisory lock held for a
// transaction is what makes a read-modify-write of one payment or one subscription exclusive.
package postgres
