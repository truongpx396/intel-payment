// Package app implements the driving ports on top of the driven ones: Admit, Record, Grant and
// Transfer (the Meter), the spend-journal replay, and — in the writer files — the sole durable
// writer. It owns the invariants; it imports no infrastructure SDK and reads no clock.
package app
