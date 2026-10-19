package vinted

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// defaultPerPage matches what the web app asks for on profile pages.
const defaultPerPage = 20

type rawUser struct {
	ID                 flexInt   `json:"id"`
	Login              string    `json:"login"`
	ProfileURL         string    `json:"profile_url"`
	CountryTitle       string    `json:"country_title"`
	CountryCode        string    `json:"country_code"`
	City               string    `json:"city"`
	ItemCount          flexInt   `json:"item_count"`
	FeedbackCount      flexInt   `json:"feedback_count"`
	FeedbackReputation flexFloat `json:"feedback_reputation"`
	FollowersCount     flexInt   `json:"followers_count"`
	FollowingCount     flexInt   `json:"following_count"`
	Business           bool      `json:"business"`
	LastLoggedOnTS     flexTime  `json:"last_loged_on_ts"`
	// Verification lists one entry per channel (email, facebook, google,
	// phone...), each with its own optional fields.
	Verification lenient[struct {
		Email lenient[struct {
			Valid bool `json:"valid"`
		}] `json:"email"`
		// Phone is only present on /users/current.
		Phone lenient[*struct {
			Valid bool `json:"valid"`
		}] `json:"phone"`
	}] `json:"verification"`
}

func (c *Client) normalizeUser(u rawUser) User {
	ver := u.Verification.V
	out := User{
		ID:        int64(u.ID),
		Login:     u.Login,
		URL:       u.ProfileURL,
		Country:   u.CountryTitle,
		City:      u.City,
		Items:     int(u.ItemCount),
		Reviews:   int(u.FeedbackCount),
		Followers: int(u.FollowersCount),
		Following: int(u.FollowingCount),
		// feedback_reputation is 0-1; the profile shows it as stars.
		Rating:        float64(u.FeedbackReputation) * 5,
		Business:      u.Business,
		EmailVerified: ver.Email.V.Valid,
		LastSeen:      u.LastLoggedOnTS.Time,
	}
	if p := ver.Phone.V; p != nil {
		out.PhoneVerified = &p.Valid
	}
	keepRaw(&out.Unparsed, "verification", u.Verification.Raw)
	keepRaw(&out.Unparsed, "verification.email", ver.Email.Raw)
	keepRaw(&out.Unparsed, "verification.phone", ver.Phone.Raw)
	if out.Country == "" {
		out.Country = u.CountryCode
	}
	if out.URL == "" && out.ID != 0 {
		out.URL = c.Base + "/member/" + strconv.FormatInt(out.ID, 10)
	}
	return out
}

// userResponse reads the {"user": {...}} answer of the users endpoints. The
// id is the one field the profile cannot do without.
func (c *Client) userResponse(endpoint string, u rawUser) (User, error) {
	if u.ID == 0 {
		return User{}, &Error{Kind: KindAPIChanged, Endpoint: endpoint, Msg: "vinted returned a profile without its id"}
	}
	return c.normalizeUser(u), nil
}

// User fetches a public profile.
func (c *Client) User(ctx context.Context, id int64) (User, error) {
	if id <= 0 {
		return User{}, Usage("user id must be positive")
	}
	path := "/api/v2/users/" + strconv.FormatInt(id, 10)
	var out struct {
		User rawUser `json:"user"`
	}
	if err := c.getJSON(ctx, path, nil, false, &out); err != nil {
		return User{}, notFoundAs(err, "user %d does not exist", id)
	}
	return c.userResponse("GET "+path, out.User)
}

// Me fetches the logged-in account.
func (c *Client) Me(ctx context.Context) (User, error) {
	const path = "/api/v2/users/current"
	var out struct {
		User rawUser `json:"user"`
	}
	if err := c.getJSON(ctx, path, nil, true, &out); err != nil {
		return User{}, err
	}
	return c.userResponse("GET "+path, out.User)
}

// rawAPIItem is the item shape of /api/v2 lists (wardrobe, favourites).
// Prices are {"amount","currency_code"} objects or bare "6.0" strings; brand,
// size and status are display strings Vinted also ships as objects in some
// lists, so they are read leniently.
type rawAPIItem struct {
	ID             flexInt              `json:"id"`
	Title          string               `json:"title"`
	UserID         flexInt              `json:"user_id"`
	Price          flexMoneyAmount      `json:"price"`
	Currency       string               `json:"currency"`
	TotalItemPrice flexMoneyAmount      `json:"total_item_price"`
	URL            string               `json:"url"`
	Path           string               `json:"path"`
	IsClosed       bool                 `json:"is_closed"`
	IsReserved     bool                 `json:"is_reserved"`
	IsHidden       bool                 `json:"is_hidden"`
	IsFavourite    bool                 `json:"is_favourite"`
	Promoted       bool                 `json:"promoted"`
	FavouriteCount flexInt              `json:"favourite_count"`
	ViewCount      flexInt              `json:"view_count"`
	Brand          lenient[string]      `json:"brand"`
	BrandTitle     string               `json:"brand_title"`
	Size           lenient[string]      `json:"size"`
	SizeTitle      string               `json:"size_title"`
	Status         lenient[string]      `json:"status"`
	CatalogID      flexInt              `json:"catalog_id"`
	Photos         lenient[[]rawPhoto]  `json:"photos"`
	Photo          lenient[*rawPhoto]   `json:"photo"`
	User           lenient[*rawUserRef] `json:"user"`
}

func (c *Client) normalizeAPIItem(r rawAPIItem) Item {
	it := Item{
		ID:         int64(r.ID),
		Title:      r.Title,
		Price:      r.Price.Amount,
		Currency:   r.Price.Currency,
		TotalPrice: r.TotalItemPrice.Amount,
		Brand:      firstNonEmpty(r.Brand.V, r.BrandTitle),
		Size:       firstNonEmpty(r.Size.V, r.SizeTitle),
		Condition:  r.Status.V,
		CategoryID: int(r.CatalogID),
		SellerID:   int64(r.UserID),
		Favorites:  int(r.FavouriteCount),
		Views:      int(r.ViewCount),
		Reserved:   r.IsReserved,
		Sold:       r.IsClosed,
		Hidden:     r.IsHidden,
		Favorited:  r.IsFavourite,
		Promoted:   r.Promoted,
		URL:        r.URL,
	}
	if it.Currency == "" {
		it.Currency = r.Currency
	}
	keepRaw(&it.Unparsed, "price", r.Price.Raw)
	keepRaw(&it.Unparsed, "total_item_price", r.TotalItemPrice.Raw)
	keepRaw(&it.Unparsed, "brand", r.Brand.Raw)
	keepRaw(&it.Unparsed, "size", r.Size.Raw)
	keepRaw(&it.Unparsed, "status", r.Status.Raw)
	keepRaw(&it.Unparsed, "photos", r.Photos.Raw)
	keepRaw(&it.Unparsed, "photo", r.Photo.Raw)
	keepRaw(&it.Unparsed, "user", r.User.Raw)
	photos := r.Photos.V
	if len(photos) == 0 && r.Photo.V != nil {
		photos = []rawPhoto{*r.Photo.V}
	}
	it.Images = photoURLs(photos)
	// The main photo's upload time is the closest thing to a listing date
	// these lists expose.
	it.CreatedAt = photoTime(photos)
	if u := r.User.V; u != nil {
		if it.SellerID == 0 {
			it.SellerID = int64(u.ID)
		}
		it.SellerLogin = u.Login
	}
	if it.URL == "" {
		it.URL = c.absURL(r.Path, it.ID)
	}
	return it
}

// rawItemList is a page of /api/v2 items. One item in an unknown shape is
// kept raw in the page's Unparsed instead of failing the page.
type rawItemList struct {
	Items      []lenient[rawAPIItem]  `json:"items"`
	Pagination lenient[rawPagination] `json:"pagination"`
}

func (c *Client) itemList(l rawItemList) SearchPage {
	p := l.Pagination.V
	out := SearchPage{Items: make([]Item, 0, len(l.Items)), Page: p.CurrentPage, TotalPages: p.TotalPages, Total: p.TotalEntries}
	keepRaw(&out.Unparsed, "pagination", l.Pagination.Raw)
	for i, r := range l.Items {
		if r.Raw != nil {
			keepRaw(&out.Unparsed, "items."+strconv.Itoa(i), r.Raw)
			continue
		}
		out.Items = append(out.Items, c.normalizeAPIItem(r.V))
	}
	return out
}

func pageQuery(page, perPage int) url.Values {
	if page <= 0 {
		page = 1
	}
	if perPage <= 0 {
		perPage = defaultPerPage
	}
	return url.Values{"page": {strconv.Itoa(page)}, "per_page": {strconv.Itoa(perPage)}}
}

// UserItems lists a member's wardrobe, newest first as on the profile.
func (c *Client) UserItems(ctx context.Context, id int64, page, perPage int) (SearchPage, error) {
	if id <= 0 {
		return SearchPage{}, Usage("user id must be positive")
	}
	q := pageQuery(page, perPage)
	q.Set("order", "relevance")
	var out rawItemList
	if err := c.getJSON(ctx, "/api/v2/wardrobe/"+strconv.FormatInt(id, 10)+"/items", q, false, &out); err != nil {
		return SearchPage{}, notFoundAs(err, "user %d does not exist", id)
	}
	return c.itemList(out), nil
}

// Favorites lists the logged-in account's favourited items.
func (c *Client) Favorites(ctx context.Context, page int) (SearchPage, error) {
	me, err := c.selfID()
	if err != nil {
		return SearchPage{}, err
	}
	var out rawItemList
	if err := c.getJSON(ctx, "/api/v2/users/"+strconv.FormatInt(me, 10)+"/items/favourites", pageQuery(page, defaultPerPage), true, &out); err != nil {
		return SearchPage{}, err
	}
	return c.itemList(out), nil
}

// ToggleFavorite flips the favourite state of an item, as the heart button
// does. Vinted only offers a toggle; read Item(...).Favorited first to know
// which way it will go.
func (c *Client) ToggleFavorite(ctx context.Context, itemID int64) error {
	if itemID <= 0 {
		return Usage("item id must be positive")
	}
	body := map[string]any{"type": "item", "user_favourites": []int64{itemID}}
	_, err := c.do(ctx, request{method: http.MethodPost, path: "/api/v2/user_favourites/toggle", body: body, auth: true, csrf: true}, nil)
	return notFoundAs(err, "item %d does not exist or was removed", itemID)
}

// selfID is the logged-in account id from the session's token.
func (c *Client) selfID() (int64, error) {
	if c.Account == nil {
		return 0, &Error{Kind: KindAuth, Msg: "this command needs a logged-in profile. Run `vinted auth login`"}
	}
	id := c.Account.UserID()
	if id == 0 {
		return 0, &Error{Kind: KindAuth, Msg: "the profile does not know its account id. Run `vinted auth login` again"}
	}
	return id, nil
}

type rawFeedback struct {
	ID             flexInt              `json:"id"`
	Rating         flexInt              `json:"rating"`
	Feedback       string               `json:"feedback"`
	CreatedAtTS    flexTime             `json:"created_at_ts"`
	CreatedAt      flexTime             `json:"created_at"`
	FeedbackUserID flexInt              `json:"feedback_user_id"`
	ItemTitle      string               `json:"item_title"`
	User           lenient[*rawUserRef] `json:"user"`
}

// Reviews lists the feedback a member received, newest first. A review in
// an unknown shape is kept whole in its entry's Unparsed.
func (c *Client) Reviews(ctx context.Context, id int64, page int) ([]Review, error) {
	if id <= 0 {
		return nil, Usage("user id must be positive")
	}
	q := pageQuery(page, defaultPerPage)
	q.Set("user_id", strconv.FormatInt(id, 10))
	var out struct {
		UserFeedbacks []lenient[rawFeedback] `json:"user_feedbacks"`
	}
	if err := c.getJSON(ctx, "/api/v2/user_feedbacks", q, false, &out); err != nil {
		return nil, notFoundAs(err, "user %d does not exist", id)
	}
	reviews := make([]Review, 0, len(out.UserFeedbacks))
	for i, lf := range out.UserFeedbacks {
		var r Review
		if lf.Raw != nil {
			keepRaw(&r.Unparsed, "user_feedbacks."+strconv.Itoa(i), lf.Raw)
			reviews = append(reviews, r)
			continue
		}
		f := lf.V
		r = Review{ID: int64(f.ID), Rating: int(f.Rating), Comment: f.Feedback, Date: f.CreatedAtTS.Time, ByID: int64(f.FeedbackUserID), ItemTitle: f.ItemTitle}
		if r.Date.IsZero() {
			r.Date = f.CreatedAt.Time
		}
		if u := f.User.V; u != nil {
			if r.ByID == 0 {
				r.ByID = int64(u.ID)
			}
			r.ByLogin = u.Login
		}
		keepRaw(&r.Unparsed, "user", f.User.Raw)
		reviews = append(reviews, r)
	}
	return reviews, nil
}

type rawCatalog struct {
	ID       flexInt               `json:"id"`
	Title    string                `json:"title"`
	Catalogs []lenient[rawCatalog] `json:"catalogs"`
}

func (r rawCatalog) category() Category {
	c := Category{ID: int(r.ID), Title: r.Title}
	for i, child := range r.Catalogs {
		if child.Raw != nil {
			keepRaw(&c.Unparsed, "catalogs."+strconv.Itoa(i), child.Raw)
			continue
		}
		c.Children = append(c.Children, child.V.category())
	}
	return c
}

// Categories returns the full category tree (the one the upload form uses,
// whose ids are the catalog[] search filter). A top-level category in an
// unknown shape is kept whole in its entry's Unparsed.
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	var out struct {
		Catalogs []lenient[rawCatalog] `json:"catalogs"`
	}
	if err := c.getJSON(ctx, "/api/v2/item_upload/catalogs", nil, false, &out); err != nil {
		return nil, err
	}
	cats := make([]Category, 0, len(out.Catalogs))
	for i, r := range out.Catalogs {
		if r.Raw != nil {
			var cat Category
			keepRaw(&cat.Unparsed, "catalogs."+strconv.Itoa(i), r.Raw)
			cats = append(cats, cat)
			continue
		}
		cats = append(cats, r.V.category())
	}
	return cats, nil
}

// Brands searches brands by name; the ids are the brand_ids[] search filter.
// A brand in an unknown shape is kept whole in its entry's Unparsed.
func (c *Client) Brands(ctx context.Context, keyword string) ([]Brand, error) {
	q := url.Values{}
	if keyword != "" {
		q.Set("keyword", keyword)
	}
	var out struct {
		Brands []lenient[struct {
			ID        flexInt `json:"id"`
			Title     string  `json:"title"`
			Slug      string  `json:"slug"`
			ItemCount flexInt `json:"item_count"`
		}] `json:"brands"`
	}
	if err := c.getJSON(ctx, "/api/v2/brands", q, false, &out); err != nil {
		return nil, err
	}
	brands := make([]Brand, 0, len(out.Brands))
	for i, lb := range out.Brands {
		if lb.Raw != nil {
			var b Brand
			keepRaw(&b.Unparsed, "brands."+strconv.Itoa(i), lb.Raw)
			brands = append(brands, b)
			continue
		}
		b := lb.V
		brands = append(brands, Brand{ID: int(b.ID), Title: b.Title, Slug: b.Slug, Items: int(b.ItemCount)})
	}
	return brands, nil
}

// notFoundAs replaces a generic KindNotFound with a human subject.
func notFoundAs(err error, format string, args ...any) error {
	var ve *Error
	if errors.As(err, &ve) && ve.Kind == KindNotFound {
		nf := NotFound(format, args...)
		nf.Status, nf.Endpoint, nf.Body = ve.Status, ve.Endpoint, ve.Body
		return nf
	}
	return err
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
