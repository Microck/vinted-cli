package vinted

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strconv"
)

// Orders the catalog page accepts.
var searchOrders = map[string]bool{"relevance": true, "newest_first": true, "price_low_to_high": true, "price_high_to_low": true}

var (
	// filterCodeRe matches the filter codes of the filter service (size,
	// video_game_rating, ...), which name the catalog keys code_ids[].
	filterCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	currencyRe   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Search runs a catalog search. vinted.com has no JSON search endpoint
// (/api/v2/catalog/items answers an HTML 404), so this loads the catalog page
// the browser would and reads its RSC payload. The page always holds up to 96
// items; PerPage only trims that client-side.
func (c *Client) Search(ctx context.Context, p SearchParams) (SearchPage, error) {
	q, err := p.query()
	if err != nil {
		return SearchPage{}, err
	}
	raw, err := c.getPage(ctx, "/catalog", q)
	if err != nil {
		return SearchPage{}, err
	}
	page, err := c.parseCatalogPage(raw)
	if err != nil {
		return SearchPage{}, err
	}
	if p.PerPage > 0 && len(page.Items) > p.PerPage {
		page.Items = page.Items[:p.PerPage]
	}
	return page, nil
}

func (p SearchParams) query() (url.Values, error) {
	q := url.Values{}
	if p.Text != "" {
		q.Set("search_text", p.Text)
	}
	addIDs := func(key string, ids []int) {
		for _, id := range ids {
			q.Add(key, strconv.Itoa(id))
		}
	}
	addIDs("catalog[]", p.CatalogIDs)
	addIDs("brand_ids[]", p.BrandIDs)
	addIDs("size_ids[]", p.SizeIDs)
	addIDs("status_ids[]", p.StatusIDs)
	addIDs("color_ids[]", p.ColorIDs)
	addIDs("material_ids[]", p.MaterialIDs)
	codes := make([]string, 0, len(p.Attributes))
	for code := range p.Attributes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		if !filterCodeRe.MatchString(code) || code == "price" || code == "catalog" {
			return nil, Usage("filter code %q is not one Vinted uses; see `vinted search filters`", code)
		}
		addIDs(code+"_ids[]", p.Attributes[code])
	}
	if p.PriceFrom < 0 || p.PriceTo < 0 {
		return nil, Usage("prices cannot be negative")
	}
	if p.PriceTo > 0 && p.PriceFrom > p.PriceTo {
		return nil, Usage("--price-from (%g) is above --price-to (%g)", p.PriceFrom, p.PriceTo)
	}
	if p.PriceFrom > 0 {
		q.Set("price_from", strconv.FormatFloat(p.PriceFrom, 'f', -1, 64))
	}
	if p.PriceTo > 0 {
		q.Set("price_to", strconv.FormatFloat(p.PriceTo, 'f', -1, 64))
	}
	if p.Currency != "" {
		if !currencyRe.MatchString(p.Currency) {
			return nil, Usage("currency %q is not a three-letter ISO code such as EUR or USD", p.Currency)
		}
		q.Set("currency", p.Currency)
	}
	if p.Order != "" {
		if !searchOrders[p.Order] {
			return nil, Usage("unknown order %q; use relevance, newest_first, price_low_to_high or price_high_to_low", p.Order)
		}
		q.Set("order", p.Order)
	}
	if p.Page < 0 || p.PerPage < 0 {
		return nil, Usage("page and per-page must be positive")
	}
	if p.Page > 0 {
		q.Set("page", strconv.Itoa(p.Page))
	}
	if p.PerPage > 0 {
		q.Set("per_page", strconv.Itoa(p.PerPage))
	}
	for k, vs := range p.Extra {
		q.Del(k)
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	return q, nil
}

// catalogProduct is one entry of the catalog page's "items" list.
type catalogProduct struct {
	ID          flexInt `json:"id"`
	ProductItem struct {
		ID             flexInt    `json:"id"`
		Title          string     `json:"title"`
		URL            string     `json:"url"`
		FavouriteCount int        `json:"favouriteCount"`
		Price          *rawMoney  `json:"price"`
		TotalItemPrice *rawMoney  `json:"totalItemPrice"`
		IsPromoted     bool       `json:"isPromoted"`
		IsFavourite    bool       `json:"isFavourite"`
		ThumbnailURLs  []string   `json:"thumbnailUrls"`
		Photos         []rawPhoto `json:"photos"`
		User           struct {
			ID flexInt `json:"id"`
		} `json:"user"`
		ItemBox struct {
			FirstLine  string `json:"firstLine"`
			SecondLine string `json:"secondLine"`
		} `json:"itemBox"`
	} `json:"productItem"`
}

func (c *Client) parseCatalogPage(raw []byte) (SearchPage, error) {
	rsc := parseRSC(raw)
	if rsc == nil {
		return SearchPage{}, c.pageShapeError("GET /catalog", raw)
	}
	holder := rsc.find(func(m map[string]any) bool {
		if !hasKeys(m, "items", "pagination") {
			return false
		}
		_, ok := m["items"].([]any)
		return ok
	})
	if holder == nil {
		return SearchPage{}, c.pageShapeError("GET /catalog", raw)
	}
	var data struct {
		Items      []catalogProduct `json:"items"`
		Pagination rawPagination    `json:"pagination"`
	}
	if err := rsc.decode(holder, &data); err != nil {
		return SearchPage{}, &Error{Kind: KindAPIChanged, Endpoint: "GET /catalog", Body: "decode: " + err.Error(), Msg: "vinted's catalog page changed shape; the cli cannot read it"}
	}
	out := SearchPage{Items: make([]Item, 0, len(data.Items)), Page: data.Pagination.CurrentPage, TotalPages: data.Pagination.TotalPages, Total: data.Pagination.TotalEntries}
	for _, ci := range data.Items {
		out.Items = append(out.Items, c.catalogItem(ci))
	}
	return out, nil
}

func (c *Client) catalogItem(ci catalogProduct) Item {
	p := ci.ProductItem
	id := int64(p.ID)
	if id == 0 {
		id = int64(ci.ID)
	}
	size, condition := splitSecondLine(p.ItemBox.SecondLine)
	images := photoURLs(p.Photos)
	if len(images) == 0 {
		images = p.ThumbnailURLs
	}
	return Item{
		ID:         id,
		Title:      p.Title,
		Price:      p.Price.amount(),
		Currency:   p.Price.currency(),
		TotalPrice: p.TotalItemPrice.amount(),
		Brand:      p.ItemBox.FirstLine,
		Size:       size,
		Condition:  condition,
		SellerID:   int64(p.User.ID),
		Favorites:  p.FavouriteCount,
		Favorited:  p.IsFavourite,
		Promoted:   p.IsPromoted,
		Images:     images,
		URL:        c.absURL(p.URL, id),
	}
}

// absURL turns a site-relative path into a link on the client's domain.
func (c *Client) absURL(path string, itemID int64) string {
	if path == "" {
		if itemID == 0 {
			return ""
		}
		path = "/items/" + strconv.FormatInt(itemID, 10)
	}
	if u, err := url.Parse(path); err == nil && u.IsAbs() {
		return path
	}
	return c.Base + path
}

// pageShapeError explains a server-rendered page without the expected data:
// an upstream redesign. Bot walls never get here; do stops them first.
func (c *Client) pageShapeError(endpoint string, raw []byte) error {
	return &Error{Kind: KindAPIChanged, Endpoint: endpoint, Body: c.snippet(raw), Msg: "vinted's page no longer carries the data the cli reads; it may have been redesigned"}
}
