package vinted

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// The catalog page renders its filter bar from the API gateway's filter
// service (GET /svc-filters/filters), the same call the web app makes when
// the category or the selection changes. The catalog page's own payload only
// names the filters, without their options. Every filter code becomes a
// catalog query key: code "size" is size_ids[], "video_game_rating" is
// video_game_rating_ids[], except price, which is price_from/price_to.

// Filter is one filter the catalog offers for a search.
type Filter struct {
	ID    int    `json:"id"`
	Code  string `json:"code"`
	Title string `json:"title"`
	// Type is how the web app draws it: list, list_search (a searchable
	// list showing only popular options, such as brand), grid or
	// hybrid_price (the price range).
	Type string `json:"type"`
	// Multi reports whether several options can be selected at once.
	Multi bool `json:"multi"`
	// Params are the catalog query keys the filter sets.
	Params  []string       `json:"params"`
	Options []FilterOption `json:"options,omitempty"`
}

// FilterOption is a selectable value of a Filter, or a heading grouping
// more options (Type group, navigational or label; those have no id).
type FilterOption struct {
	ID    int64  `json:"id,omitempty"`
	Title string `json:"title"`
	// Type is empty for a selectable option.
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	// Count is how many listings of the search have this option.
	Count int `json:"count,omitempty"`
	// Hex and Image are the swatch the site shows (colours, patterns).
	Hex     string         `json:"hex,omitempty"`
	Image   string         `json:"image,omitempty"`
	Options []FilterOption `json:"options,omitempty"`
}

// Selectable reports whether the option's id can be passed to the catalog.
func (o FilterOption) Selectable() bool { return o.Type == "" }

// FilterParam is the catalog query key a filter code sets.
func FilterParam(code string) []string {
	if code == "price" {
		return []string{"price_from", "price_to", "currency"}
	}
	return []string{code + "_ids[]"}
}

type rawFilter struct {
	ID            flexInt `json:"id"`
	Title         string  `json:"title"`
	Code          string  `json:"code"`
	DisplayType   string  `json:"display_type"`
	SelectionType string  `json:"selection_type"`
	IsHidden      bool    `json:"is_hidden"`
	// IsLazy filters come without options; the web app loads them from
	// /svc-filters/filters/facets when the filter is opened.
	IsLazy  bool              `json:"is_lazy"`
	Options []rawFilterOption `json:"options"`
}

type rawFilterOption struct {
	ID          flexInt `json:"id"`
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Type        string  `json:"type"`
	ItemsCount  int     `json:"items_count"`
	Prefix      *struct {
		Hex string `json:"hex"`
		URL string `json:"url"`
	} `json:"prefix"`
	Options []rawFilterOption `json:"options"`
}

// Filters lists the filters the catalog offers for a search: the filter
// codes, how many options can be picked, and the option ids with titles.
// The text, catalog ids and any selection in p narrow them exactly as on the
// site (sizes depend on the category, options on what is already picked).
//
// The list itself is one request; most filters then need one more each for
// their options, as in the web app. codes limits which filters' options are
// loaded (all when empty); the others are listed without options.
func (c *Client) Filters(ctx context.Context, p SearchParams, codes ...string) ([]Filter, error) {
	q, err := p.filterQuery()
	if err != nil {
		return nil, err
	}
	var out struct {
		Filters []rawFilter `json:"filters"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/svc-filters/filters", query: q, gateway: true}, &out); err != nil {
		return nil, err
	}
	if out.Filters == nil {
		return nil, &Error{Kind: KindAPIChanged, Endpoint: "GET /svc-filters/filters", Msg: "vinted's filter list came back without filters; the cli cannot read it"}
	}
	want := func(code string) bool {
		for _, c := range codes {
			if c == code {
				return true
			}
		}
		return len(codes) == 0
	}
	known := map[string]bool{}
	lazy := map[string]bool{}
	filters := make([]Filter, 0, len(out.Filters))
	for _, rf := range out.Filters {
		if rf.IsHidden || rf.Code == "" {
			continue
		}
		known[rf.Code] = true
		f := Filter{
			ID: int(rf.ID), Code: rf.Code, Title: rf.Title, Type: rf.DisplayType,
			Multi: rf.SelectionType == "multi", Params: FilterParam(rf.Code), Options: filterOptions(rf.Options),
		}
		lazy[rf.Code] = rf.IsLazy && len(f.Options) == 0 && rf.Code != "price"
		filters = append(filters, f)
	}
	for _, code := range codes {
		if !known[code] {
			return nil, Usage("vinted offers no %q filter for this search; the codes are the first column of `vinted search filters`", code)
		}
	}
	for i, f := range filters {
		if lazy[f.Code] && want(f.Code) {
			if filters[i].Options, err = c.filterFacets(ctx, q, f.Code); err != nil {
				return nil, err
			}
		}
	}
	return filters, nil
}

// filterFacets loads the options of one lazy filter.
func (c *Client) filterFacets(ctx context.Context, q url.Values, code string) ([]FilterOption, error) {
	fq := url.Values{}
	for k, vs := range q {
		fq[k] = vs
	}
	fq.Set("filter_code", code)
	var out struct {
		Options []rawFilterOption `json:"options"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/svc-filters/filters/facets", query: fq, gateway: true}, &out); err != nil {
		return nil, err
	}
	return filterOptions(out.Options), nil
}

func filterOptions(raw []rawFilterOption) []FilterOption {
	if len(raw) == 0 {
		return nil
	}
	out := make([]FilterOption, 0, len(raw))
	for _, r := range raw {
		o := FilterOption{ID: int64(r.ID), Title: r.Title, Description: r.Description, Count: r.ItemsCount, Options: filterOptions(r.Options)}
		if r.Type != "" && r.Type != "default" {
			// Headings carry string ids ("WOMEN-LT") the catalog does not take.
			o.Type, o.ID = r.Type, 0
		}
		if r.Prefix != nil {
			o.Hex, o.Image = r.Prefix.Hex, r.Prefix.URL
		}
		out = append(out, o)
	}
	return out
}

// filterQuery is the filter service's form of a search: the catalog and
// every selected filter go in attribute_ids[code] as comma-joined ids, the
// rest as on the catalog page.
func (p SearchParams) filterQuery() (url.Values, error) {
	cq, err := p.query()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	attrs := map[string][]string{}
	for k, vs := range cq {
		switch {
		case k == "catalog[]":
			attrs["catalog"] = append(attrs["catalog"], vs...)
		case strings.HasSuffix(k, "_ids[]"):
			code := strings.TrimSuffix(k, "_ids[]")
			attrs[code] = append(attrs[code], vs...)
		case k == "search_text" || k == "price_from" || k == "price_to" || k == "currency" || k == "order":
			q[k] = vs
		}
	}
	codes := make([]string, 0, len(attrs))
	for code := range attrs {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		ids := attrs[code]
		for _, id := range ids {
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				return nil, Usage("%s id %q is not a number", code, id)
			}
		}
		q["attribute_ids["+code+"]"] = []string{strings.Join(ids, ",")}
	}
	return q, nil
}
