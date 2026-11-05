package events

import "time"

// Retries exposes the schedule to the external tests, which hold the spread to every step of it.
func Retries() []time.Duration { return append([]time.Duration(nil), retries[:]...) }
