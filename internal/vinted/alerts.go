package vinted

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// Saved searches live on the API gateway's search-bar service, next to the
// recent searches the search box remembers: both are "previous searches",
// told apart by type (type_saved is a subscribed search, the alert Vinted
// notifies about; type_recent is history).

// SavedSearch is a search the account saved (or, with All, merely ran).
type SavedSearch struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Subtitle  string    `json:"subtitle,omitempty"`
	Saved     bool      `json:"saved"`
	NewItems  int       `json:"new_items"`
	LastVisit time.Time `json:"last_visited_at,omitzero"`
	// Query is the search the saved one runs, ready for Search.
	Query SearchParams `json:"query"`
	// Unparsed holds, by JSON path, parts of the saved search in a shape
	// the cli does not know ("selected_filters.0", "price_from"), or the
	// whole entry ("previous_searches.N") when it cannot be read at all.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

type rawPreviousSearch struct {
	ID            flexInt         `json:"id"`
	Title         string          `json:"title"`
	Subtitle      string          `json:"subtitle"`
	Type          string          `json:"type"`
	SearchText    string          `json:"search_text"`
	CatalogID     flexInt         `json:"catalog_id"`
	PriceFrom     flexMoneyAmount `json:"price_from"`
	PriceTo       flexMoneyAmount `json:"price_to"`
	NewItemsCount flexInt         `json:"new_items_count"`
	LastVisitedAt flexTime        `json:"last_visited_at"`
	// SelectedFilters are the search's catalog filters, one entry per filter
	// code; an entry or id in an unknown shape is kept raw.
	SelectedFilters lenient[[]lenient[rawSelectedFilter]] `json:"selected_filters"`
}

type rawSelectedFilter struct {
	Code string            `json:"code"`
	IDs  []json.RawMessage `json:"ids"`
}

func (r rawPreviousSearch) normalize() SavedSearch {
	s := SavedSearch{
		ID: int64(r.ID), Title: r.Title, Subtitle: r.Subtitle, Saved: r.Type == "type_saved",
		NewItems: int(r.NewItemsCount), LastVisit: r.LastVisitedAt.Time,
	}
	keepRaw(&s.Unparsed, "price_from", r.PriceFrom.Raw)
	keepRaw(&s.Unparsed, "price_to", r.PriceTo.Raw)
	keepRaw(&s.Unparsed, "selected_filters", r.SelectedFilters.Raw)
	q := SearchParams{Text: r.SearchText, PriceFrom: r.PriceFrom.Amount, PriceTo: r.PriceTo.Amount}
	if r.CatalogID != 0 {
		q.CatalogIDs = []int{int(r.CatalogID)}
	}
	for i, lf := range r.SelectedFilters.V {
		path := "selected_filters." + strconv.Itoa(i)
		if lf.Raw != nil {
			keepRaw(&s.Unparsed, path, lf.Raw)
			continue
		}
		f := lf.V
		ids := make([]int, 0, len(f.IDs))
		for j, raw := range f.IDs {
			id, ok := filterID(raw)
			if !ok {
				keepRaw(&s.Unparsed, path+".ids."+strconv.Itoa(j), raw)
				continue
			}
			ids = append(ids, id)
		}
		switch f.Code {
		case "brand":
			q.BrandIDs = append(q.BrandIDs, ids...)
		case "size":
			q.SizeIDs = append(q.SizeIDs, ids...)
		case "status":
			q.StatusIDs = append(q.StatusIDs, ids...)
		case "color":
			q.ColorIDs = append(q.ColorIDs, ids...)
		case "material":
			q.MaterialIDs = append(q.MaterialIDs, ids...)
		default:
			// Any other catalog filter is an attribute: Search sends it as
			// CODE_ids[], and watch listings describe it by code.
			if q.Attributes == nil {
				q.Attributes = map[string][]int{}
			}
			q.Attributes[f.Code] = append(q.Attributes[f.Code], ids...)
		}
	}
	s.Query = q
	return s
}

// filterID reads a filter option id given as 12, 12.0 or "12". ok is false
// for any other shape (an option object, a non-numeric string), which the
// caller keeps raw rather than searching for id 0.
func filterID(raw json.RawMessage) (int, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 1 || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func (c *Client) searchBarPath() (string, error) {
	me, err := c.selfID()
	if err != nil {
		return "", err
	}
	return "/search-bar/v2/users/" + strconv.FormatInt(me, 10) + "/previous_searches", nil
}

// SavedSearches lists the account's saved searches, or with all also the
// recent searches the search box remembers. An entry in an unknown shape is
// listed either way, kept whole in its Unparsed, since its type is unknown.
func (c *Client) SavedSearches(ctx context.Context, all bool) ([]SavedSearch, error) {
	path, err := c.searchBarPath()
	if err != nil {
		return nil, err
	}
	var out struct {
		PreviousSearches []lenient[rawPreviousSearch] `json:"previous_searches"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path, auth: true, gateway: true}, &out); err != nil {
		return nil, err
	}
	list := make([]SavedSearch, 0, len(out.PreviousSearches))
	for i, r := range out.PreviousSearches {
		if r.Raw != nil {
			var s SavedSearch
			keepRaw(&s.Unparsed, "previous_searches."+strconv.Itoa(i), r.Raw)
			list = append(list, s)
			continue
		}
		s := r.V.normalize()
		if all || s.Saved {
			list = append(list, s)
		}
	}
	return list, nil
}

// SavedSearch loads one saved (or recent) search by id.
func (c *Client) SavedSearch(ctx context.Context, id int64) (SavedSearch, error) {
	if id <= 0 {
		return SavedSearch{}, Usage("saved search id must be positive")
	}
	path, err := c.searchBarPath()
	if err != nil {
		return SavedSearch{}, err
	}
	var out struct {
		PreviousSearch *rawPreviousSearch `json:"previous_search"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path + "/" + strconv.FormatInt(id, 10), auth: true, gateway: true}, &out); err != nil {
		return SavedSearch{}, notFoundAs(err, "saved search %d does not exist. See `vinted alert list`", id)
	}
	if out.PreviousSearch == nil || out.PreviousSearch.ID == 0 {
		return SavedSearch{}, NotFound("saved search %d does not exist. See `vinted alert list`", id)
	}
	return out.PreviousSearch.normalize(), nil
}
