package vinted

import (
	"context"
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
}

type rawPreviousSearch struct {
	ID              flexInt   `json:"id"`
	Title           string    `json:"title"`
	Subtitle        string    `json:"subtitle"`
	Type            string    `json:"type"`
	SearchText      string    `json:"search_text"`
	CatalogID       flexInt   `json:"catalog_id"`
	PriceFrom       *rawMoney `json:"price_from"`
	PriceTo         *rawMoney `json:"price_to"`
	NewItemsCount   int       `json:"new_items_count"`
	LastVisitedAt   flexTime  `json:"last_visited_at"`
	SelectedFilters []struct {
		Code string    `json:"code"`
		IDs  []flexInt `json:"ids"`
	} `json:"selected_filters"`
}

func (r rawPreviousSearch) normalize() SavedSearch {
	s := SavedSearch{
		ID: int64(r.ID), Title: r.Title, Subtitle: r.Subtitle, Saved: r.Type == "type_saved",
		NewItems: r.NewItemsCount, LastVisit: r.LastVisitedAt.Time,
	}
	q := SearchParams{Text: r.SearchText, PriceFrom: r.PriceFrom.amount(), PriceTo: r.PriceTo.amount()}
	if r.CatalogID != 0 {
		q.CatalogIDs = []int{int(r.CatalogID)}
	}
	for _, f := range r.SelectedFilters {
		ids := make([]int, 0, len(f.IDs))
		for _, id := range f.IDs {
			ids = append(ids, int(id))
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

func (c *Client) searchBarPath() (string, error) {
	me, err := c.selfID()
	if err != nil {
		return "", err
	}
	return "/search-bar/v2/users/" + strconv.FormatInt(me, 10) + "/previous_searches", nil
}

// SavedSearches lists the account's saved searches, or with all also the
// recent searches the search box remembers.
func (c *Client) SavedSearches(ctx context.Context, all bool) ([]SavedSearch, error) {
	path, err := c.searchBarPath()
	if err != nil {
		return nil, err
	}
	var out struct {
		PreviousSearches []rawPreviousSearch `json:"previous_searches"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: path, auth: true, gateway: true}, &out); err != nil {
		return nil, err
	}
	list := make([]SavedSearch, 0, len(out.PreviousSearches))
	for _, r := range out.PreviousSearches {
		s := r.normalize()
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
