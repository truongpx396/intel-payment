package app

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/truongpx396/intel-payment/events"
)

// Feed limits.
const (
	DefaultFeedLimit = 100
	MaxFeedLimit     = 500
	maxFeedTypes     = 20
)

var typePattern = regexp.MustCompile(`^[a-z][a-z_]*(\.[a-z][a-z_]*)+$`)

// Feed is the pull door onto the log: for hosts behind a firewall, batch consumers, and a backfill of what a failed endpoint missed.
type Feed struct{ store events.Store }

// NewFeed builds the feed over the log.
func NewFeed(store events.Store) (*Feed, error) {
	if store == nil {
		return nil, errors.New("events feed: a Store is required")
	}
	return &Feed{store: store}, nil
}

// Page is one page of the feed.
type Page struct {
	Events []events.Event
	// Next is the cursor to resume from: the last event of the page, or the cursor the caller gave when the page is empty (nothing
	// new yet, and the caller's place is unchanged). Empty only for a caller that has never had an event.
	Next string
}

// Page reads the events of realms after a cursor, oldest first, `limit` at a time (DefaultFeedLimit when 0, at most MaxFeedLimit).
// A consumer stores the last id it processed and resumes from it; there is no server-side cursor to lose, and every event is in
// exactly one page of a consumer that does.
func (f *Feed) Page(ctx context.Context, realms []string, after string, types []string, limit int) (Page, error) {
	switch {
	case len(realms) == 0:
		return Page{}, fmt.Errorf("%w: a feed is read for at least one realm", events.ErrInvalid)
	case after != "" && !events.ValidID(after):
		return Page{}, fmt.Errorf("%w: after must be an event id", events.ErrInvalid)
	case limit < 0 || limit > MaxFeedLimit:
		return Page{}, fmt.Errorf("%w: limit is 1 to %d", events.ErrInvalid, MaxFeedLimit)
	case len(types) > maxFeedTypes:
		return Page{}, fmt.Errorf("%w: at most %d types", events.ErrInvalid, maxFeedTypes)
	}
	for _, t := range types {
		if !typePattern.MatchString(t) {
			return Page{}, fmt.Errorf("%w: %q is not an event type", events.ErrInvalid, t)
		}
	}
	if limit == 0 {
		limit = DefaultFeedLimit
	}
	evs, err := f.store.Feed(ctx, events.FeedQuery{Realms: realms, After: after, Types: types, Limit: limit})
	if err != nil {
		return Page{}, err
	}
	next := after
	if len(evs) > 0 {
		next = evs[len(evs)-1].ID
	}
	return Page{Events: evs, Next: next}, nil
}
