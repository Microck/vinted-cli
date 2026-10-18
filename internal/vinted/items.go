package vinted

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var (
	itemPathRe  = regexp.MustCompile(`/items/(\d+)`)
	userPathRe  = regexp.MustCompile(`/member/(?:general/)?(\d+)`)
	canonicalRe = regexp.MustCompile(`<link rel="canonical" href="([^"]+)"`)
)

// ParseItemRef accepts a numeric item id or any Vinted item URL
// (https://www.vinted.fr/items/123-slug, with or without query, any domain).
func ParseItemRef(ref string) (int64, error) {
	return parseRef(ref, itemPathRe, "item", "https://www.vinted.com/items/1234567890-title")
}

// ParseUserRef accepts a numeric member id or a profile URL
// (https://www.vinted.fr/member/123-login).
func ParseUserRef(ref string) (int64, error) {
	return parseRef(ref, userPathRe, "user", "https://www.vinted.com/member/1234567-login")
}

func parseRef(ref string, re *regexp.Regexp, what, example string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, Usage("missing %s id or URL", what)
	}
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil && id > 0 {
		return id, nil
	}
	if u, err := url.Parse(ref); err == nil && (u.Host != "" || strings.HasPrefix(ref, "/")) {
		if u.Host != "" && !isVintedHost(u.Hostname()) {
			return 0, Usage("%q is not a vinted URL", ref)
		}
		if m := re.FindStringSubmatch(u.Path); m != nil {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
				return id, nil
			}
		}
	}
	return 0, Usage("%q is not a valid %s id or URL; expected a number or a link like %s", ref, what, example)
}

// isVintedHost reports whether host (without port) is one of Vinted's own
// sites: vinted.<tld> or www.vinted.<tld> for a TLD Vinted runs. Anything
// else, including look-alikes such as vinted.evil.com, is rejected.
func isVintedHost(host string) bool {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if host == "vinted.com" {
		return true
	}
	_, ok := siteLanguages[host]
	return ok
}

// itemSidebar is the item object the page's sidebar is rendered from; its
// photos, price and plugins are RSC references resolved before decoding.
type itemSidebar struct {
	ID         flexInt            `json:"id"`
	Title      string             `json:"title"`
	CatalogID  flexInt            `json:"catalog_id"`
	Currency   string             `json:"currency"`
	SellerID   flexInt            `json:"seller_id"`
	Business   bool               `json:"business"`
	IsHidden   bool               `json:"is_hidden"`
	IsReserved bool               `json:"is_reserved"`
	CanBuy     *bool              `json:"can_buy"`
	Price      lenient[*rawMoney] `json:"price"`
	Photos     lenient[[]struct {
		URL           string `json:"url"`
		Width         int    `json:"width"`
		Height        int    `json:"height"`
		IsMain        bool   `json:"is_main"`
		DominantColor string `json:"dominant_color_opaque"`
	}] `json:"photos"`
	BrandDTO lenient[*struct {
		ID    flexInt `json:"id"`
		Title string  `json:"title"`
	}] `json:"brand_dto"`
	Plugins lenient[[]itemPlugin] `json:"plugins"`
}

// itemPlugin is one block of the item page. Data stays raw and is decoded
// only by the plugin that owns its schema, so one plugin's odd field cannot
// break the others.
type itemPlugin struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

type descriptionPlugin struct {
	Description string `json:"description"`
}

type attributesPlugin struct {
	Attributes []struct {
		Code string `json:"code"`
		Data struct {
			ID    flexInt `json:"id"`
			Title string  `json:"title"`
			Value string  `json:"value"`
		} `json:"data"`
	} `json:"attributes"`
}

// userInfoHeaderPlugin is the seller box.
type userInfoHeaderPlugin struct {
	Name               string                     `json:"name"`
	Business           bool                       `json:"business"`
	FeedbackCount      flexInt                    `json:"feedback_count"`
	FeedbackReputation flexFloat                  `json:"feedback_reputation"`
	UserInfo           lenient[[]userInfoLine]    `json:"user_info"`
	Badges             lenient[[]json.RawMessage] `json:"badges"`
}

type userInfoLine struct {
	Key  string `json:"key"`
	Text string `json:"text"`
}

type sellerBadgesPlugin struct {
	Badges lenient[[]json.RawMessage] `json:"badges"`
}

// badgeName reads a badge given as a plain string ("Pro") or as an object
// naming it ({"type": "..."}, or a title or name). ok is false for any other
// shape, which the caller keeps raw.
// addBadges appends the badges it can name and keeps the others raw under
// key.
func addBadges(names []string, badges lenient[[]json.RawMessage], key string, unparsed *map[string]json.RawMessage) []string {
	keepRaw(unparsed, key, badges.Raw)
	for i, b := range badges.V {
		if n, ok := badgeName(b); ok {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		} else if string(b) != "null" {
			keepRaw(unparsed, key+"."+strconv.Itoa(i), b)
		}
	}
	return names
}

func badgeName(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, s != ""
	}
	var o struct {
		Type  string `json:"type"`
		Title string `json:"title"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return "", false
	}
	for _, v := range []string{o.Type, o.Title, o.Name} {
		if v != "" {
			return v, true
		}
	}
	return "", false
}

// itemPricing is the page's price breakdown (pricingServices), the same
// numbers the sidebar shows under the price.
type itemPricing struct {
	Services struct {
		BuyerProtection *struct {
			FinalPrice *rawMoney `json:"finalPrice"`
		} `json:"buyerProtection"`
		Shipping *rawShipping `json:"shipping"`
	} `json:"services"`
	TotalAmount *rawMoney `json:"totalAmount"`
}

type rawShipping struct {
	OriginalPrice *rawMoney `json:"originalPrice"`
	FinalPrice    *rawMoney `json:"finalPrice"`
	IsFree        bool      `json:"isFree"`
	Multiple      bool      `json:"areMultipleShippingOptionsAvailable"`
	DeliveryType  string    `json:"deliveryType"`
}

// shipping applies the web app's rule (its transformPricingServiceSvcResponse):
// free only with a single option; with several, the final price is not
// computed (0) and the original price is the cheapest option, shown as
// "from". A single paid option costs its final, discounted price.
func (s *rawShipping) shipping() *Shipping {
	sh := &Shipping{Free: s.IsFree, FromPrice: s.Multiple, Delivery: s.DeliveryType, Currency: s.OriginalPrice.currency()}
	switch {
	case s.IsFree:
	case s.Multiple || s.FinalPrice.amount() == 0:
		sh.Price = s.OriginalPrice.amount()
	default:
		sh.Price, sh.Currency = s.FinalPrice.amount(), s.FinalPrice.currency()
	}
	return sh
}

var catalogPathRe = regexp.MustCompile(`^/catalog/(\d+)`)

// Item loads an item page and reads its RSC payload; Vinted has no public
// JSON endpoint for a single item. Views are not shown on the page, and the
// listing date only as relative text (Uploaded), so CreatedAt stays empty.
func (c *Client) Item(ctx context.Context, id int64) (Item, error) {
	if id <= 0 {
		return Item{}, Usage("item id must be positive")
	}
	path := "/items/" + strconv.FormatInt(id, 10)
	raw, err := c.getPage(ctx, path, nil)
	if err != nil {
		var ve *Error
		if errors.As(err, &ve) && ve.Kind == KindNotFound {
			return Item{}, NotFound("item %d does not exist or was removed", id)
		}
		return Item{}, err
	}
	return c.parseItemPage(raw, id)
}

func (c *Client) parseItemPage(raw []byte, id int64) (Item, error) {
	endpoint := "GET /items/" + strconv.FormatInt(id, 10)
	rsc := parseRSC(raw)
	if rsc == nil {
		return Item{}, c.pageShapeError(endpoint, raw)
	}
	idStr := strconv.FormatInt(id, 10)
	sameItem := func(m map[string]any) bool {
		v := m["id"]
		if v == nil {
			v = m["item_id"]
		}
		switch t := v.(type) {
		case string:
			return t == idStr
		case interface{ String() string }:
			return t.String() == idStr
		}
		return false
	}
	holder := rsc.find(func(m map[string]any) bool {
		return hasKeys(m, "seller_id", "catalog_id", "plugins", "title") && sameItem(m)
	})
	if holder == nil {
		// A removed item renders a generic page without the item payload.
		if rsc.find(func(m map[string]any) bool { return hasKeys(m, "catalog_id") }) == nil {
			return Item{}, NotFound("item %d does not exist or was removed", id)
		}
		return Item{}, c.pageShapeError(endpoint, raw)
	}
	var side itemSidebar
	if err := rsc.decode(holder, &side); err != nil {
		return Item{}, &Error{Kind: KindAPIChanged, Endpoint: endpoint, Body: "decode: " + err.Error(), Msg: "vinted's item page changed shape; the cli cannot read it"}
	}
	price := side.Price.V
	it := Item{
		ID:         int64(side.ID),
		Title:      side.Title,
		Price:      price.amount(),
		Currency:   price.currency(),
		CategoryID: int(side.CatalogID),
		SellerID:   int64(side.SellerID),
		Reserved:   side.IsReserved,
		Hidden:     side.IsHidden,
		CanBuy:     side.CanBuy,
		URL:        c.absURL("", id),
	}
	keepRaw(&it.Unparsed, "price", side.Price.Raw)
	keepRaw(&it.Unparsed, "photos", side.Photos.Raw)
	keepRaw(&it.Unparsed, "brand_dto", side.BrandDTO.Raw)
	keepRaw(&it.Unparsed, "plugins", side.Plugins.Raw)
	if it.Currency == "" {
		it.Currency = side.Currency
	}
	for _, p := range side.Photos.V {
		if p.URL == "" {
			continue
		}
		it.Images = append(it.Images, p.URL)
		it.Photos = append(it.Photos, ItemPhoto{URL: p.URL, OriginalWidth: p.Width, OriginalHeight: p.Height, Main: p.IsMain, DominantColor: p.DominantColor})
	}
	if b := side.BrandDTO.V; b != nil {
		it.Brand, it.BrandID = b.Title, int(b.ID)
	}
	seller := &ItemSeller{ID: it.SellerID, Business: side.Business}
	for i, p := range side.Plugins.V {
		key := "plugins." + p.Name
		if _, dup := it.Unparsed[key]; dup || p.Name == "" {
			key += "." + strconv.Itoa(i)
		}
		// unparsed keeps a plugin's data whole when its schema did not match.
		unparsed := func(err error) bool {
			if err != nil {
				keepRaw(&it.Unparsed, key, p.Data)
			}
			return err != nil
		}
		switch p.Name {
		case "description":
			var d descriptionPlugin
			if !unparsed(json.Unmarshal(p.Data, &d)) {
				it.Description = d.Description
			}
		case "attributes":
			var d attributesPlugin
			if unparsed(json.Unmarshal(p.Data, &d)) {
				continue
			}
			for _, a := range d.Attributes {
				it.Attributes = append(it.Attributes, ItemAttribute{Code: a.Code, Title: a.Data.Title, Value: a.Data.Value})
				switch a.Code {
				case "size":
					it.Size = a.Data.Value
				case "status":
					it.Condition = a.Data.Value
				case "color":
					it.Color = a.Data.Value
				case "upload_date":
					it.Uploaded = a.Data.Value
				case "brand":
					if it.Brand == "" {
						it.Brand, it.BrandID = a.Data.Value, int(a.Data.ID)
					}
				}
			}
		case "user_info_header":
			var d userInfoHeaderPlugin
			if unparsed(json.Unmarshal(p.Data, &d)) {
				continue
			}
			it.SellerLogin = d.Name
			seller.Login, seller.Business = d.Name, d.Business
			seller.Reviews = int(d.FeedbackCount)
			// The web app draws feedback_reputation (0-1) as round(5*r, 2) stars.
			seller.Rating = math.Round(float64(d.FeedbackReputation)*5*100) / 100
			keepRaw(&it.Unparsed, key+".user_info", d.UserInfo.Raw)
			for _, ui := range d.UserInfo.V {
				if ui.Text == "" {
					continue
				}
				seller.Info = append(seller.Info, ui.Text)
				if ui.Key == "last-logged-in" {
					seller.LastSeen = ui.Text
				}
			}
			seller.Badges = addBadges(seller.Badges, d.Badges, key+".badges", &it.Unparsed)
		case "seller_badges_info":
			var d sellerBadgesPlugin
			if !unparsed(json.Unmarshal(p.Data, &d)) {
				seller.Badges = addBadges(seller.Badges, d.Badges, key+".badges", &it.Unparsed)
			}
		case "buyer_item_status":
			// The status banner replaces the buy buttons. Reserved and hidden
			// items carry their own flags; any other status means sold.
			if !it.Reserved && !it.Hidden {
				it.Sold = true
			}
		case "breadcrumbs", "favourite", "shipping", "summary":
			// Placeholders: their content is read from the page's own
			// objects below (breadcrumbs, favourites, pricing).
		default:
			// A block the cli has no reader for still reaches the output.
			if len(p.Data) > 0 && string(p.Data) != "null" {
				keepRaw(&it.Unparsed, key, p.Data)
			}
		}
	}
	if fav := rsc.find(func(m map[string]any) bool {
		return hasKeys(m, "favourite_count", "is_favourite", "item_id") && sameItem(m)
	}); fav != nil {
		var f struct {
			FavouriteCount int   `json:"favourite_count"`
			IsFavourite    *bool `json:"is_favourite"`
		}
		if rsc.decode(fav, &f) == nil {
			it.Favorites = f.FavouriteCount
			if f.IsFavourite != nil {
				it.Favorited, it.FavoritedKnown = *f.IsFavourite, true
			}
		}
	}
	if crumbs := rsc.find(func(m map[string]any) bool {
		return hasKeys(m, "breadcrumbs", "item_id") && sameItem(m)
	}); crumbs != nil {
		var b struct {
			Breadcrumbs []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
			} `json:"breadcrumbs"`
		}
		if rsc.decode(crumbs, &b) == nil {
			for _, cr := range b.Breadcrumbs {
				// The last crumb narrows the category to the item's brand
				// (/catalog/1845-straight-jeans/brand/10-levis); not a category.
				m := catalogPathRe.FindStringSubmatch(cr.URL)
				if m == nil || strings.Contains(cr.URL, "/brand/") {
					continue
				}
				cid, _ := strconv.Atoi(m[1])
				it.CategoryPath = append(it.CategoryPath, CategoryRef{ID: cid, Title: cr.Title})
			}
		}
	}
	if pricing := rsc.find(func(m map[string]any) bool { return hasKeys(m, "services", "totalAmount") }); pricing != nil {
		var pr itemPricing
		if rsc.decode(pricing, &pr) == nil {
			it.TotalPrice = pr.TotalAmount.amount()
			if bp := pr.Services.BuyerProtection; bp != nil {
				it.BuyerProtectionFee = bp.FinalPrice.amount()
			}
			if s := pr.Services.Shipping; s != nil {
				it.Shipping = s.shipping()
			}
		}
	} else if total := rsc.find(func(m map[string]any) bool { return hasKeys(m, "totalAmount") }); total != nil {
		var t struct {
			TotalAmount *rawMoney `json:"totalAmount"`
		}
		if rsc.decode(total, &t) == nil {
			it.TotalPrice = t.TotalAmount.amount()
		}
	}
	if it.SellerLogin == "" {
		it.SellerLogin = sellerLoginFromPage(rsc, it.SellerID)
		seller.Login = it.SellerLogin
	}
	if it.SellerID != 0 {
		it.Seller = seller
	}
	// The canonical link carries the slugged URL the site itself shares.
	if m := canonicalRe.FindSubmatch(raw); m != nil && strings.Contains(string(m[1]), "/items/"+strconv.FormatInt(id, 10)) {
		it.URL = string(m[1])
	}
	return it, nil
}

// MaxPhotoBytes bounds one photo, downloaded or uploaded; Vinted's f800
// photos are well under a megabyte.
const MaxPhotoBytes = 32 << 20

// Photo downloads one item photo (an Images URL) from Vinted's image CDN
// and returns its bytes and content type. It passes the Gate like every
// other request, so downloads are paced and count against the command's
// request budget. Only Vinted image hosts are fetched.
func (c *Client) Photo(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || !c.photoHost(u) {
		return nil, "", Usage("%q is not a vinted photo URL", rawURL)
	}
	endpoint := "GET " + u.Host + u.Path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	h := req.Header
	h.Set("User-Agent", browserUA)
	h.Set("Sec-Ch-Ua", browserCHUA)
	h.Set("Sec-Ch-Ua-Mobile", "?0")
	h.Set("Sec-Ch-Ua-Platform", browserPlatform)
	h.Set("Accept-Language", c.acceptLanguage())
	h.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
	h.Set("Sec-Fetch-Dest", "image")
	h.Set("Sec-Fetch-Mode", "no-cors")
	h.Set("Sec-Fetch-Site", "cross-site")
	h.Set("Referer", c.Base+"/")
	if c.Gate != nil {
		if err := c.Gate.Wait(ctx); err != nil {
			return nil, "", err
		}
	}
	c.debugf("> GET %s", c.redact(u.String()))
	// The image CDN carries no cookies, so it may redirect within its host.
	cdn := *c.HTTP
	cdn.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects || req.URL.Host != via[0].URL.Host {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := cdn.Do(req)
	if err != nil {
		return nil, "", c.netError(endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxPhotoBytes+1))
	if err != nil {
		return nil, "", c.netError(endpoint, err)
	}
	c.debugf("< %d %s (%d bytes)", resp.StatusCode, endpoint, len(raw))
	// The CDN answers a bad or expired signature with a plain 403, which is
	// not the bot protection; only a challenge page or a rate limit is.
	if resp.StatusCode == http.StatusTooManyRequests || isBotWall(raw) {
		return nil, "", c.blocked(ctx, endpoint, response{status: resp.StatusCode, header: resp.Header, raw: raw})
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", c.statusError(endpoint, resp.StatusCode, raw)
	}
	if len(raw) > MaxPhotoBytes {
		return nil, "", &Error{Kind: KindAPIChanged, Endpoint: endpoint, Msg: "the photo is larger than the cli accepts"}
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "image/") {
		return nil, "", &Error{Kind: KindAPIChanged, Status: resp.StatusCode, Endpoint: endpoint, Body: c.snippet(raw), Msg: "vinted's image host answered something that is not an image"}
	}
	return raw, ct, nil
}

// photoHost accepts Vinted's image CDN (images1.vinted.net, ...) and the
// site's own host, which is what tests serve from.
func (c *Client) photoHost(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if strings.HasSuffix(host, ".vinted.net") {
		return u.Scheme == "https"
	}
	base, err := url.Parse(c.Base)
	return err == nil && strings.EqualFold(base.Host, u.Host) && base.Scheme == u.Scheme
}

// sellerLoginFromPage falls back to any user object for the seller.
func sellerLoginFromPage(rsc *rscPage, sellerID int64) string {
	if sellerID == 0 {
		return ""
	}
	want := strconv.FormatInt(sellerID, 10)
	m := rsc.find(func(m map[string]any) bool {
		if !hasKeys(m, "id", "login") {
			return false
		}
		switch t := m["id"].(type) {
		case string:
			return t == want
		case interface{ String() string }:
			return t.String() == want
		}
		return false
	})
	if m == nil {
		return ""
	}
	s, _ := m["login"].(string)
	return s
}
