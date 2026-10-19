package watch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Microck/vinted-cli/internal/store"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// catalogServer serves the vinted testdata catalog page with its two item
// ids swapped for the ones the test puts on the page.
type catalogServer struct {
	mu   sync.Mutex
	page [2]int64
}

func (s *catalogServer) set(a, b int64) {
	s.mu.Lock()
	s.page = [2]int64{a, b}
	s.mu.Unlock()
}

func newCatalogServer(t *testing.T) (*catalogServer, *vinted.Client) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "vinted", "testdata", "catalog.html"))
	if err != nil {
		t.Fatal(err)
	}
	cs := &catalogServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			// The anonymous bootstrap only needs a guest access token.
			http.SetCookie(w, &http.Cookie{Name: vinted.AccessCookie, Value: "guest"})
			_, _ = w.Write([]byte("<html></html>"))
		case "/catalog":
			cs.mu.Lock()
			page := cs.page
			cs.mu.Unlock()
			body := strings.NewReplacer("1000000001", strconv.FormatInt(page[0], 10), "1000000002", strconv.FormatInt(page[1], 10)).Replace(string(raw))
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := vinted.New()
	c.Base = srv.URL
	return cs, c
}

func newSearchWatch(t *testing.T) (*store.Store, store.Watch) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	target, _ := json.Marshal(SearchTarget{Params: vinted.SearchParams{Text: "nike", Order: "newest_first"}})
	w, err := st.AddWatch(context.Background(), store.Watch{Name: "s", Profile: "default", Kind: store.KindSearch, Target: target, Interval: 300e9})
	if err != nil {
		t.Fatal(err)
	}
	return st, w
}

// check runs and commits one check the way the CLI does, returning the
// item.new and item.removed events by item id. The fake page reuses the
// fixture's two listings by position, so price changes are artifacts.
func check(t *testing.T, c *vinted.Client, st *store.Store, name string, emitInitial bool) map[int64]string {
	t.Helper()
	ctx := context.Background()
	w, err := st.Watch(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Check(ctx, c, st, w, emitInitial)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitCheck(ctx, w.ID, res.SinceID, res.Upserts, res.Removed, res.Events); err != nil {
		t.Fatal(err)
	}
	out := map[int64]string{}
	for _, ev := range res.Events {
		if ev.Type != "item.new" && ev.Type != "item.removed" {
			continue
		}
		var it vinted.Item
		_ = json.Unmarshal(ev.Item, &it)
		if prev, dup := out[it.ID]; dup {
			t.Fatalf("item %d got %s and %s in one check", it.ID, prev, ev.Type)
		}
		out[it.ID] = ev.Type
	}
	return out
}

// Vinted's result pages reshuffle between loads: older listings the first
// check never saw surface, seen ones drop off and come back. Only listings
// newer than the watch may be item.new, and nothing that merely left the
// page is reported.
func TestSearchWatchIgnoresReshuffledOldListings(t *testing.T) {
	cs, c := newCatalogServer(t)
	st, w := newSearchWatch(t)

	cs.set(1000000005, 1000000003)
	if evs := check(t, c, st, w.Name, false); len(evs) != 0 {
		t.Fatalf("baseline reported %v", evs)
	}
	if w, _ = st.Watch(context.Background(), w.Name); w.SinceID != 1000000005 {
		t.Fatalf("since_id = %d", w.SinceID)
	}

	// An older listing surfaces and a seen one drops off: no events.
	cs.set(1000000003, 1000000001)
	if evs := check(t, c, st, w.Name, false); len(evs) != 0 {
		t.Fatalf("reshuffle reported %v", evs)
	}

	// A listing newer than the watch is new, once.
	cs.set(1000000009, 1000000001)
	if evs := check(t, c, st, w.Name, false); len(evs) != 1 || evs[1000000009] != "item.new" {
		t.Fatalf("new listing events = %v", evs)
	}
	cs.set(1000000005, 1000000009)
	if evs := check(t, c, st, w.Name, false); len(evs) != 0 {
		t.Fatalf("listings drifting back reported %v", evs)
	}

	// A new listing missed by one load (id below another new one) still
	// counts, because the threshold stays at the first check's newest id.
	cs.set(1000000007, 1000000003)
	if evs := check(t, c, st, w.Name, false); len(evs) != 1 || evs[1000000007] != "item.new" {
		t.Fatalf("late new listing events = %v", evs)
	}
	if w, _ = st.Watch(context.Background(), w.Name); w.SinceID != 1000000005 {
		t.Fatalf("since_id moved to %d", w.SinceID)
	}
}

func TestSearchWatchEmitInitialReportsEverythingOnce(t *testing.T) {
	cs, c := newCatalogServer(t)
	st, w := newSearchWatch(t)
	cs.set(1000000005, 1000000003)
	if evs := check(t, c, st, w.Name, true); len(evs) != 2 {
		t.Fatalf("emit-initial reported %v", evs)
	}
	cs.set(1000000003, 1000000001)
	if evs := check(t, c, st, w.Name, false); len(evs) != 0 {
		t.Fatalf("reshuffle after emit-initial reported %v", evs)
	}
}

func TestSearchWatchDeduplicatesRepeatedItemsWithinCheck(t *testing.T) {
	cs, c := newCatalogServer(t)
	st, w := newSearchWatch(t)
	cs.set(1000000009, 1000000009)
	result, err := Check(context.Background(), c, st, w, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || len(result.Upserts) != 1 {
		t.Fatalf("duplicate item produced %d events and %d upserts", len(result.Events), len(result.Upserts))
	}
	var eventItem, upsertItem vinted.Item
	if err := json.Unmarshal(result.Events[0].Item, &eventItem); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Upserts[0].Snapshot, &upsertItem); err != nil {
		t.Fatal(err)
	}
	for name, item := range map[string]vinted.Item{"event": eventItem, "upsert": upsertItem} {
		if item.Title != "Nike Air Max 90" || item.Price != 45 {
			t.Fatalf("%s used a later duplicate: title %q, price %v", name, item.Title, item.Price)
		}
	}
}
