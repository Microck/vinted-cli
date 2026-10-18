package vinted

import (
	"context"
	"encoding/json"
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
	// Unparsed keeps, by dotted JSON path, the raw parts of the filter the
	// cli could not read. A whole filter entry it could not read appears as
	// a Filter without a code whose Unparsed holds it under "filters.N".
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
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
	// Unparsed keeps, by dotted JSON path, the raw parts of the option the
	// cli could not read.
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
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

// rawFilter is one entry of the filter list. Only the code is core: it names
// the catalog key. The rest is lenient so one odd field keeps the filter.
type rawFilter struct {
	ID            flexInt         `json:"id"`
	Title         lenient[string] `json:"title"`
	Code          string          `json:"code"`
	DisplayType   lenient[string] `json:"display_type"`
	SelectionType lenient[string] `json:"selection_type"`
	IsHidden      lenient[bool]   `json:"is_hidden"`
	// IsLazy filters come without options; the web app loads them from
	// /svc-filters/filters/facets when the filter is opened.
	IsLazy  lenient[bool]             `json:"is_lazy"`
	Options lenient[rawFilterOptions] `json:"options"`
}

// rawFilterOptions keeps each option lenient on its own, so one option of
// another shape does not cost the filter its other options.
type rawFilterOptions []lenient[rawFilterOption]

// rawFilterOption is one option or heading. Its id and title are core.
type rawFilterOption struct {
	ID          flexInt                   `json:"id"`
	Title       string                    `json:"title"`
	Description lenient[string]           `json:"description"`
	Type        lenient[string]           `json:"type"`
	ItemsCount  flexInt                   `json:"items_count"`
	Prefix      lenient[*rawFilterPrefix] `json:"prefix"`
	Options     lenient[rawFilterOptions] `json:"options"`
}

type rawFilterPrefix struct {
	Hex string `json:"hex"`
	URL string `json:"url"`
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
		Filters []lenient[rawFilter] `json:"filters"`
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
	for i, lf := range out.Filters {
		if lf.Raw != nil {
			f := Filter{}
			keepRaw(&f.Unparsed, "filters."+strconv.Itoa(i), lf.Raw)
			filters = append(filters, f)
			continue
		}
		rf := lf.V
		if rf.IsHidden.V || rf.Code == "" {
			continue
		}
		known[rf.Code] = true
		f := Filter{
			ID: int(rf.ID), Code: rf.Code, Title: rf.Title.V, Type: rf.DisplayType.V,
			Multi: rf.SelectionType.V == "multi", Params: FilterParam(rf.Code),
		}
		f.Options = filterOptions(rf.Options, &f.Unparsed, "options")
		keepRaw(&f.Unparsed, "title", rf.Title.Raw)
		keepRaw(&f.Unparsed, "display_type", rf.DisplayType.Raw)
		keepRaw(&f.Unparsed, "selection_type", rf.SelectionType.Raw)
		keepRaw(&f.Unparsed, "is_hidden", rf.IsHidden.Raw)
		keepRaw(&f.Unparsed, "is_lazy", rf.IsLazy.Raw)
		lazy[rf.Code] = rf.IsLazy.V && len(f.Options) == 0 && rf.Code != "price"
		filters = append(filters, f)
	}
	for _, code := range codes {
		if !known[code] {
			return nil, Usage("vinted offers no %q filter for this search; the codes are the first column of `vinted search filters`", code)
		}
	}
	for i, f := range filters {
		if lazy[f.Code] && want(f.Code) {
			if err := c.filterFacets(ctx, q, &filters[i]); err != nil {
				return nil, err
			}
		}
	}
	return filters, nil
}

// filterFacets loads the options of one lazy filter into f. Options it
// cannot read go to f.Unparsed under "facets.options".
func (c *Client) filterFacets(ctx context.Context, q url.Values, f *Filter) error {
	fq := url.Values{}
	for k, vs := range q {
		fq[k] = vs
	}
	fq.Set("filter_code", f.Code)
	var out struct {
		Options lenient[rawFilterOptions] `json:"options"`
	}
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/svc-filters/filters/facets", query: fq, gateway: true}, &out); err != nil {
		return err
	}
	f.Options = filterOptions(out.Options, &f.Unparsed, "facets.options")
	return nil
}

// filterOptions converts the options found at path, recording in *unparsed
// the list or the entries it could not read.
func filterOptions(list lenient[rawFilterOptions], unparsed *map[string]json.RawMessage, path string) []FilterOption {
	keepRaw(unparsed, path, list.Raw)
	if len(list.V) == 0 {
		return nil
	}
	out := make([]FilterOption, 0, len(list.V))
	for i, lo := range list.V {
		if lo.Raw != nil {
			keepRaw(unparsed, path+"."+strconv.Itoa(i), lo.Raw)
			continue
		}
		r := lo.V
		o := FilterOption{ID: int64(r.ID), Title: r.Title, Description: r.Description.V, Count: int(r.ItemsCount)}
		o.Options = filterOptions(r.Options, &o.Unparsed, "options")
		if t := r.Type.V; t != "" && t != "default" {
			// Headings carry string ids ("WOMEN-LT") the catalog does not take.
			o.Type, o.ID = t, 0
		}
		if p := r.Prefix.V; p != nil {
			o.Hex, o.Image = p.Hex, p.URL
		}
		keepRaw(&o.Unparsed, "description", r.Description.Raw)
		keepRaw(&o.Unparsed, "type", r.Type.Raw)
		keepRaw(&o.Unparsed, "prefix", r.Prefix.Raw)
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
