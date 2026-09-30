package domain

import (
	"fmt"
	"strconv"
	"time"
)

// A window limit's counter is a Redis string per bucket. This file decides, purely from a limit and
// an instant, WHICH buckets exist: the ones to read when admitting, and the one to increment (with
// its lifetime) when settling. Keeping it pure keeps it testable at DST changes and month ends.

// RollingBuckets is how many sub-buckets a Rolling window is divided into. The error of a rolling
// counter is at most one bucket, i.e. Dur/RollingBuckets.
const RollingBuckets = 12

// DefaultJobTTL is how long a job's counter lives after its first charge. A Job limit's own Dur is
// known only at Admit, where the budget is passed; the counter is created at settlement, which does
// not see it, so it lives for a fixed generous time and an abandoned job leaks only a TTL'd key.
const DefaultJobTTL = 24 * time.Hour

// counterMargin is added to every counter's lifetime so a bucket never expires before its window
// has fully ended.
const counterMargin = time.Hour

// Location resolves Limit.TZ, defaulting to UTC.
func (l Limit) Location() (*time.Location, error) {
	if l.TZ == "" || l.TZ == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(l.TZ)
	if err != nil {
		return nil, fmt.Errorf("%w: limit %q has unknown time zone %q", ErrInvalid, l.Name, l.TZ)
	}
	return loc, nil
}

// Bucket is one counter bucket: the key suffix that names it and how long it must live.
type Bucket struct {
	Key string
	TTL time.Duration
}

// ReadBuckets returns every bucket an Admit must sum for l at instant now. Daily and Hourly have
// one; Rolling has RollingBuckets; Job has one, unbucketed.
func (l Limit) ReadBuckets(now time.Time) ([]string, error) {
	switch l.Window {
	case Daily, Hourly:
		b, err := l.WriteBucket(now)
		return []string{b.Key}, err
	case Rolling:
		w := l.rollingWidth()
		cur := now.UnixNano() / int64(w)
		keys := make([]string, 0, RollingBuckets)
		for i := int64(0); i < RollingBuckets; i++ {
			keys = append(keys, strconv.FormatInt(cur-i, 10))
		}
		return keys, nil
	case Job:
		return []string{"-"}, nil
	}
	return nil, fmt.Errorf("%w: a %s limit has no counter", ErrInvalid, l.Window)
}

// WriteBucket is the bucket a settlement at instant now increments, and its lifetime.
func (l Limit) WriteBucket(now time.Time) (Bucket, error) {
	switch l.Window {
	case Daily:
		loc, err := l.Location()
		if err != nil {
			return Bucket{}, err
		}
		t := now.In(loc)
		next := time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		// A zone can skip a calendar date (Samoa went from 29 to 31 December 2011). Midnight of a date
		// that does not exist resolves to an instant already behind us, and a negative TTL deletes the
		// counter at once — the day's ceiling would never bind. Take the next local midnight that is ahead.
		if !next.After(now) {
			next = time.Date(t.Year(), t.Month(), t.Day()+2, 0, 0, 0, 0, loc)
		}
		return Bucket{Key: t.Format("20060102"), TTL: next.Sub(now) + counterMargin}, nil
	case Hourly:
		loc, err := l.Location()
		if err != nil {
			return Bucket{}, err
		}
		t := now.In(loc)
		// The zone offset is part of the key: on the day clocks fall back, "01:00" happens twice.
		next := time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		if !next.After(now) {
			next = now.Truncate(time.Hour).Add(time.Hour)
		}
		return Bucket{Key: t.Format("2006010215Z0700"), TTL: next.Sub(now) + counterMargin}, nil
	case Rolling:
		w := l.rollingWidth()
		return Bucket{Key: strconv.FormatInt(now.UnixNano()/int64(w), 10), TTL: l.Dur + w}, nil
	case Job:
		return Bucket{Key: "-", TTL: DefaultJobTTL}, nil
	}
	return Bucket{}, fmt.Errorf("%w: a %s limit has no counter", ErrInvalid, l.Window)
}

func (l Limit) rollingWidth() time.Duration {
	w := l.Dur / RollingBuckets
	if w < time.Second {
		w = time.Second
	}
	return w
}
