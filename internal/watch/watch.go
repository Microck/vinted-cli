// Package watch runs one Check for a Watch: fetch the target's current state,
// diff it against what the store already has, and produce Events. It does not
// deliver or persist anything; the CLI layer sequences sinks then CommitCheck.
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Microck/vinted-cli/internal/store"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// Result is everything a Check produced, ready for delivery and commit.
type Result struct {
	Events  []store.Event
	Upserts []store.Seen
	Removed []int64
	// SinceID is the newest item id this check saw; the store keeps the one
	// from the watch's first check.
	SinceID int64
}

// Target payloads stored as JSON in the watch row.
type SearchTarget struct {
	Params vinted.SearchParams `json:"params"`
}

type ItemTarget struct {
	ID  int64  `json:"id"`
	URL string `json:"url,omitempty"`
}

type SellerTarget struct {
	ID    int64  `json:"id"`
	Login string `json:"login,omitempty"`
}

// sellerPerPage is the wardrobe page size a seller watch reads; it keeps the
// number of requests per check low for large wardrobes.
const sellerPerPage = 96

// Request caps per check, so one watch cannot turn into a scrape: a search
// watch reads at most MaxSearchPages result pages, a seller watch at most
// MaxSellerPages wardrobe pages (960 listings).
const (
	MaxSearchPages = 5
	MaxSellerPages = 10
)

// Check runs one check. emitInitial makes the first run report every item as
// new instead of silently recording a baseline.
func Check(ctx context.Context, c *vinted.Client, st *store.Store, w store.Watch, emitInitial bool) (Result, error) {
	seen, err := st.SeenFor(ctx, w.ID)
	if err != nil {
		return Result{}, err
	}
	baseline := !w.Baselined && !emitInitial
	switch w.Kind {
	case store.KindSearch:
		var t SearchTarget
		if err := json.Unmarshal(w.Target, &t); err != nil {
			return Result{}, fmt.Errorf("watch %s has a corrupt target: %w", w.Name, err)
		}
		return checkSearch(ctx, c, w, t, seen, baseline)
	case store.KindItem:
		var t ItemTarget
		if err := json.Unmarshal(w.Target, &t); err != nil {
			return Result{}, fmt.Errorf("watch %s has a corrupt target: %w", w.Name, err)
		}
		return checkItem(ctx, c, w, t, seen, baseline)
	case store.KindSeller:
		var t SellerTarget
		if err := json.Unmarshal(w.Target, &t); err != nil {
			return Result{}, fmt.Errorf("watch %s has a corrupt target: %w", w.Name, err)
		}
		return checkSeller(ctx, c, w, t, seen, baseline)
	}
	return Result{}, fmt.Errorf("watch %s has unknown kind %q", w.Name, w.Kind)
}

func event(w store.Watch, typ string, it vinted.Item, change any) store.Event {
	ev := store.Event{Type: typ, Watch: w.Name, Profile: w.Profile, At: time.Now().UTC()}
	ev.Item, _ = json.Marshal(it)
	if change != nil {
		ev.Change, _ = json.Marshal(change)
	}
	return ev
}

func seenOf(it vinted.Item) store.Seen {
	snap, _ := json.Marshal(it)
	return store.Seen{ID: it.ID, Price: it.Price, Reserved: it.Reserved, Sold: it.Sold, Title: it.Title, Snapshot: snap}
}

type priceChange struct {
	From float64 `json:"from"`
	To   float64 `json:"to"`
}

// diffKnown produces the events for an item the watch has seen before.
// Sold is only trusted when the source reports it (item pages and wardrobes
// do; catalog search drops sold items instead of flagging them).
func diffKnown(w store.Watch, prev store.Seen, it vinted.Item, trustSold bool) []store.Event {
	var evs []store.Event
	if it.Price != prev.Price {
		evs = append(evs, event(w, "item.price_changed", it, priceChange{From: prev.Price, To: it.Price}))
	}
	if it.Reserved && !prev.Reserved {
		evs = append(evs, event(w, "item.reserved", it, nil))
	}
	if !it.Reserved && prev.Reserved {
		evs = append(evs, event(w, "item.unreserved", it, nil))
	}
	if trustSold && it.Sold && !prev.Sold {
		evs = append(evs, event(w, "item.sold", it, nil))
	}
	return evs
}

// checkSearch reports item.new only for listings newer than the watch.
// Vinted's catalog pages are not a stable window: even newest_first mixes in
// bumped and older items, and back-to-back loads swap listings in and out
// across the whole page. An unseen id is therefore not proof of a new
// listing. Item ids grow with listing time, so an unseen item is new only
// when its id is above w.SinceID, the newest id at the first check; older
// ones are recorded silently. Every id ever seen is kept, so each listing is
// reported once even when it drifts off the page and back.
func checkSearch(ctx context.Context, c *vinted.Client, w store.Watch, t SearchTarget, seen map[int64]store.Seen, baseline bool) (Result, error) {
	var res Result
	// Pages may overlap while the marketplace reorders results. The first
	// occurrence owns this check: emit and persist each item only once.
	visited := make(map[int64]bool)
	// Watches stored before the cap existed may ask for more pages.
	pages := min(max(w.Pages, 1), MaxSearchPages)
	params := t.Params
	for page := 1; page <= pages; page++ {
		params.Page = page
		sp, err := c.Search(ctx, params)
		if err != nil {
			return Result{}, err
		}
		for _, it := range sp.Items {
			if visited[it.ID] {
				continue
			}
			visited[it.ID] = true
			prev, known := seen[it.ID]
			switch {
			case known:
				res.Events = append(res.Events, diffKnown(w, prev, it, false)...)
			case baseline:
			case !w.Baselined || it.ID > w.SinceID:
				// The first check with --emit-initial reports everything.
				res.Events = append(res.Events, event(w, "item.new", it, nil))
			}
			res.Upserts = append(res.Upserts, seenOf(it))
			res.SinceID = max(res.SinceID, it.ID)
		}
		if len(sp.Items) == 0 || sp.Page >= sp.TotalPages {
			break
		}
	}
	// Items that fell off the watched pages are not reported: a search watch
	// tracks arrivals and changes, not departures, since the pages churn.
	return res, nil
}

func checkItem(ctx context.Context, c *vinted.Client, w store.Watch, t ItemTarget, seen map[int64]store.Seen, baseline bool) (Result, error) {
	var res Result
	it, err := c.Item(ctx, t.ID)
	if err != nil {
		var e *vinted.Error
		if errors.As(err, &e) && e.Kind == vinted.KindNotFound {
			// Gone entirely. Report once, using the last snapshot for the payload.
			if prev, ok := seen[t.ID]; ok {
				var last vinted.Item
				_ = json.Unmarshal(prev.Snapshot, &last)
				res.Events = append(res.Events, event(w, "item.removed", last, nil))
				res.Removed = append(res.Removed, t.ID)
			}
			return res, nil
		}
		return Result{}, err
	}
	prev, known := seen[it.ID]
	if known {
		res.Events = append(res.Events, diffKnown(w, prev, it, true)...)
		if it.Hidden && !wasHidden(prev) {
			res.Events = append(res.Events, event(w, "item.removed", it, nil))
		}
	} else if !baseline {
		res.Events = append(res.Events, event(w, "item.new", it, nil))
	}
	res.Upserts = append(res.Upserts, seenOf(it))
	return res, nil
}

func wasHidden(prev store.Seen) bool {
	var last struct {
		Hidden bool `json:"hidden"`
	}
	_ = json.Unmarshal(prev.Snapshot, &last)
	return last.Hidden
}

func checkSeller(ctx context.Context, c *vinted.Client, w store.Watch, t SellerTarget, seen map[int64]store.Seen, baseline bool) (Result, error) {
	var res Result
	var items []vinted.Item
	complete := false
	for page := 1; page <= MaxSellerPages; page++ {
		sp, err := c.UserItems(ctx, t.ID, page, sellerPerPage)
		if err != nil {
			return Result{}, err
		}
		items = append(items, sp.Items...)
		if len(sp.Items) == 0 || sp.Page >= sp.TotalPages {
			complete = true
			break
		}
	}
	current := map[int64]bool{}
	for _, it := range items {
		current[it.ID] = true
		prev, known := seen[it.ID]
		switch {
		case !known && !baseline:
			res.Events = append(res.Events, event(w, "seller.new_item", it, nil))
		case known:
			res.Events = append(res.Events, diffKnown(w, prev, it, true)...)
		}
		res.Upserts = append(res.Upserts, seenOf(it))
	}
	// Only a wardrobe read in full proves a missing id is gone; past
	// MaxSellerPages the tail is unknown, so nothing is reported removed.
	if !complete {
		return res, nil
	}
	for id, prev := range seen {
		if !current[id] {
			var last vinted.Item
			_ = json.Unmarshal(prev.Snapshot, &last)
			res.Events = append(res.Events, event(w, "item.removed", last, nil))
			res.Removed = append(res.Removed, id)
		}
	}
	return res, nil
}

// Jitter spreads checks so several watches on one timer do not align. Up to
// 10% of the interval, never negative.
func Jitter(interval time.Duration, seed int64) time.Duration {
	if interval <= 0 {
		return 0
	}
	max := int64(interval / 10)
	if max == 0 {
		return 0
	}
	if seed < 0 {
		seed = -seed
	}
	return time.Duration(seed % max)
}
