// Package ports is the fiat layer's seams: the provider (who takes the money), the stores (where the facts
// live), the tax strategy (who computes tax) and the two driving ports (the ingress that persists a verified
// webhook, the processor that fulfils it). Billing mints credits through metering's ports only.
package ports
