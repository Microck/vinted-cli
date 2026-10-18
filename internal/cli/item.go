package cli

import (
	"context"
	"fmt"
	"io"
	"mime"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// imageURLs keeps the machine formats as a simple array and pretty output
// suitable for copying or piping, without labels or shortened links.
type imageURLs []string

func (urls imageURLs) Pretty(w io.Writer, color bool) {
	for _, url := range urls {
		fmt.Fprintln(w, url)
	}
}

// savedPhoto is one photo `item images --download` wrote.
type savedPhoto struct {
	URL         string `json:"url"`
	File        string `json:"file"`
	Bytes       int    `json:"bytes"`
	ContentType string `json:"content_type"`
}

type savedPhotos []savedPhoto

func (v savedPhotos) Pretty(w io.Writer, color bool) {
	for _, p := range v {
		fmt.Fprintf(w, "%s  %s\n", p.File, output.Dim(fmt.Sprintf("%d bytes, %s", p.Bytes, p.ContentType), color))
	}
}

type itemView vinted.Item

func itemState(it vinted.Item) string {
	switch {
	case it.Sold:
		return "sold"
	case it.Reserved:
		return "reserved"
	case it.Hidden:
		return "hidden"
	}
	return "available"
}

func money(amount float64, currency string) string {
	return strings.TrimSpace(fmt.Sprintf("%.2f %s", amount, currency))
}

func shippingText(s *vinted.Shipping) string {
	var t string
	switch {
	case s.Free:
		t = "free"
	case s.FromPrice:
		t = "from " + money(s.Price, s.Currency) + ", depending on the option picked at checkout"
	default:
		t = money(s.Price, s.Currency)
	}
	if s.Delivery != "" {
		t += " (delivery: " + s.Delivery + ")"
	}
	return t
}

// itemAttributeShown lists the attribute codes itemView prints on their own
// rows; any other attribute (material, measurements...) is printed as is.
var itemAttributeShown = map[string]bool{"brand": true, "size": true, "status": true, "color": true, "upload_date": true}

func (v itemView) Pretty(w io.Writer, color bool) {
	it := vinted.Item(v)
	price := money(it.Price, it.Currency)
	if it.TotalPrice > 0 && it.TotalPrice != it.Price {
		extra := fmt.Sprintf(" (%s with buyer protection", money(it.TotalPrice, it.Currency))
		if it.BuyerProtectionFee > 0 {
			extra += fmt.Sprintf(" of %s", money(it.BuyerProtectionFee, it.Currency))
		}
		price += output.Dim(extra+")", color)
	}
	rows := [][]string{
		{"id", fmt.Sprint(it.ID)},
		{"title", it.Title},
		{"price", price},
	}
	if it.Shipping != nil {
		rows = append(rows, []string{"shipping", shippingText(it.Shipping)})
	}
	rows = append(rows, []string{"state", itemState(it)})
	for _, kv := range [][2]string{{"brand", it.Brand}, {"size", it.Size}, {"condition", it.Condition}, {"color", it.Color}} {
		if kv[1] != "" {
			rows = append(rows, []string{kv[0], kv[1]})
		}
	}
	for _, a := range it.Attributes {
		if !itemAttributeShown[a.Code] && a.Value != "" {
			rows = append(rows, []string{strings.ToLower(a.Title), a.Value})
		}
	}
	if len(it.CategoryPath) > 0 {
		titles := make([]string, len(it.CategoryPath))
		for i, c := range it.CategoryPath {
			titles[i] = c.Title
		}
		rows = append(rows, []string{"category", strings.Join(titles, " > ") + output.Dim(fmt.Sprintf(" (%d)", it.CategoryID), color)})
	} else if it.CategoryID != 0 {
		rows = append(rows, []string{"category", fmt.Sprint(it.CategoryID)})
	}
	if it.Uploaded != "" {
		rows = append(rows, []string{"uploaded", it.Uploaded})
	}
	if !it.CreatedAt.IsZero() {
		rows = append(rows, []string{"listed", it.CreatedAt.Local().Format(time.RFC3339)})
	}
	if it.SellerID != 0 {
		rows = append(rows, []string{"seller", strings.TrimSpace(it.SellerLogin + " " + output.Dim(fmt.Sprint(it.SellerID), color))})
	}
	if s := it.Seller; s != nil {
		line := fmt.Sprintf("%.1f/5 (%d reviews)", s.Rating, s.Reviews)
		if s.Business {
			line += ", business"
		}
		rows = append(rows, []string{"", line})
		for _, info := range s.Info {
			rows = append(rows, []string{"", info})
		}
		if p := s.Profile; p != nil {
			if loc := strings.Trim(p.City+", "+p.Country, ", "); loc != "" {
				rows = append(rows, []string{"", "located in " + loc})
			}
			if !p.LastSeen.IsZero() {
				rows = append(rows, []string{"", "last login " + p.LastSeen.Local().Format(time.RFC3339)})
			}
			rows = append(rows, []string{"", fmt.Sprintf("%d items, %d followers", p.Items, p.Followers)})
		}
	}
	switch {
	case it.Views > 0:
		rows = append(rows, []string{"views/favs", fmt.Sprintf("%d / %d", it.Views, it.Favorites)})
	case it.Favorites > 0 || it.Favorited:
		favs := fmt.Sprint(it.Favorites)
		if it.Favorited {
			favs += " (including yours)"
		}
		rows = append(rows, []string{"favorites", favs})
	}
	rows = append(rows, []string{"url", it.URL})
	if len(it.Photos) > 0 {
		for i, p := range it.Photos {
			size := ""
			if p.OriginalWidth > 0 && p.OriginalHeight > 0 {
				size = output.Dim(fmt.Sprintf(" (original %dx%d)", p.OriginalWidth, p.OriginalHeight), color)
			}
			rows = append(rows, []string{fmt.Sprintf("photo %d", i+1), p.URL + size})
		}
	} else {
		for i, url := range it.Images {
			rows = append(rows, []string{fmt.Sprintf("image %d", i+1), url})
		}
	}
	output.Table(w, color, nil, rows)
	if it.Description != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, it.Description)
	}
}

// item fetches ITEM, given as a numeric id or any Vinted item URL.
func (a *App) item(ctx context.Context, ref string) (vinted.Item, error) {
	id, err := vinted.ParseItemRef(ref)
	if err != nil {
		return vinted.Item{}, err
	}
	return a.Client.Item(ctx, id)
}

// downloadPhotos saves the item's photos into dir as ITEMID-N.EXT, one
// request each through the client's gate.
func (a *App) downloadPhotos(ctx context.Context, it vinted.Item, dir string, limit int) (savedPhotos, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, output.Usagef("cannot create --download directory: %v", err)
	}
	photos := it.Photos
	if len(photos) == 0 {
		for _, u := range it.Images {
			photos = append(photos, vinted.ItemPhoto{URL: u})
		}
	}
	if limit > 0 && len(photos) > limit {
		photos = photos[:limit]
	}
	saved := savedPhotos{}
	for i, p := range photos {
		data, ct, err := a.Client.Photo(ctx, p.URL)
		if err != nil {
			return saved, err
		}
		ext := ".jpg"
		if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 {
			ext = exts[0]
			for _, e := range exts {
				if e == ".jpg" || e == ".webp" || e == ".png" {
					ext = e
					break
				}
			}
		}
		file := filepath.Join(dir, fmt.Sprintf("%d-%d%s", it.ID, i+1, ext))
		if err := os.WriteFile(file, data, 0o644); err != nil {
			return saved, fmt.Errorf("save %s: %w", file, err)
		}
		saved = append(saved, savedPhoto{URL: p.URL, File: file, Bytes: len(data), ContentType: ct})
	}
	return saved, nil
}

func (a *App) itemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "item",
		Short: "Inspect and act on an item",
		Long: `Inspect and act on an item. ITEM is the numeric item id or an item URL
from any Vinted domain (https://www.vinted.fr/items/123-slug).`,
	}
	var withSeller bool
	show := &cobra.Command{
		Use: "show ITEM", Short: "Everything the item page shows", Args: cobra.ExactArgs(1),
		Long: `Show everything the item page shows: price with the buyer protection fee
and total, shipping, all photos (with the original upload size), description,
brand, size, condition, colour and every other detail line (material,
measurements... whichever the item has), the category path, when it was
uploaded (as the page words it: "6 hours ago"), favourites, status flags and
the seller box (login, rating, reviews, last seen). Vinted's item page shows
neither views nor an exact upload date.

--seller also loads the seller's public profile (city, country, exact last
login, items, followers): one more request.

Examples:
  vinted item show 4815162342 --format pretty
  vinted item show https://www.vinted.com/items/4815162342-jeans --seller
  vinted item show 4815162342 | jq '{price, total_price, shipping}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			it, err := a.item(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if withSeller && it.Seller != nil {
				u, err := a.Client.User(cmd.Context(), it.SellerID)
				if err != nil {
					return err
				}
				it.Seller.Profile = &u
			}
			return a.Printer.Print(itemView(it))
		},
	}
	show.Flags().BoolVar(&withSeller, "seller", false, "also load the seller's public profile (one more request)")
	var downloadDir string
	var downloadLimit int
	images := &cobra.Command{
		Use: "images ITEM", Short: "Image links in gallery order, or download them", Args: cobra.ExactArgs(1),
		Long: `Return direct image URLs for an item id or full item URL, in gallery order.
JSON returns an array of URL strings; JSONL returns one JSON string per line;
--format pretty prints one plain URL per line. ` + "`item show`" + ` lists the same
photos with the size of the uploaded original. The links are the largest
rendition the page offers (f800), which can be smaller than the original:
a 1200x1600 upload is served at 600x800.

--download DIR saves the photos into DIR as ITEMID-N.webp (N from 1, the
extension from the image type) and prints what it wrote instead. Every photo
is one request to Vinted's image host, paced and counted like any other
request; --limit caps how many are fetched.

Examples:
  vinted item images 4815162342 --format pretty
  vinted item images 4815162342 --download ./photos --limit 3
  vinted item show 4815162342 | jq -r '.images[]'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if downloadLimit < 0 {
				return output.Usagef("--limit cannot be negative")
			}
			if cmd.Flags().Changed("limit") && downloadDir == "" {
				return output.Usagef("--limit only applies with --download")
			}
			it, err := a.item(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if downloadDir != "" {
				saved, err := a.downloadPhotos(cmd.Context(), it, downloadDir, downloadLimit)
				if err != nil {
					return err
				}
				return a.Printer.Print(saved)
			}
			// A successful item with no images is [], never JSON null.
			urls := append(imageURLs{}, it.Images...)
			return a.Printer.Print(urls)
		},
	}
	images.Flags().StringVar(&downloadDir, "download", "", "save the photos into this `dir`")
	images.Flags().IntVar(&downloadLimit, "limit", 0, "with --download, fetch at most this many photos (0 = all)")
	cmd.AddCommand(
		show,
		images,
		&cobra.Command{
			Use: "open ITEM", Short: "Open the item in the browser", Args: cobra.ExactArgs(1),
			Long: `Open the item in the browser. ITEM must be an item id or an http(s) item
URL on a Vinted domain; a URL opens as given, a bare id as
<profile site>/items/ID, which Vinted redirects to the full item URL.
Nothing is requested from Vinted.`,
			RunE: func(cmd *cobra.Command, args []string) error {
				ref := strings.TrimSpace(args[0])
				u, err := neturl.Parse(ref)
				if err == nil && u.Scheme != "" && u.Scheme != "https" && u.Scheme != "http" {
					return output.Usagef("%q is not an http(s) link; item open only opens Vinted item pages", ref)
				}
				id, err := vinted.ParseItemRef(ref)
				if err != nil {
					return err
				}
				target := fmt.Sprintf("%s/items/%d", strings.TrimRight(a.Client.Base, "/"), id)
				if u != nil && u.Host != "" && u.Scheme != "" {
					// ParseItemRef only accepts Vinted hosts.
					target = u.String()
				}
				return openBrowser(target)
			},
		},
		a.favoriteCmd("favorite", "Add the item to your favorites", true),
		a.favoriteCmd("unfavorite", "Remove the item from your favorites", false),
	)
	cmd.AddCommand(a.ownItemCmds()...)
	return cmd
}

type actionResult struct {
	Action string `json:"action"`
	Item   int64  `json:"item"`
	OK     bool   `json:"ok"`
}

func (r actionResult) Pretty(w io.Writer, color bool) { fmt.Fprintf(w, "%s %d\n", r.Action, r.Item) }

// favoriteCmd sets the favorite flag to on. Vinted only offers a toggle, so
// the item's current state is read first and an item already in the wanted
// state is left alone: running the command twice never flips it back. When
// the page no longer states the current state, nothing is toggled.
func (a *App) favoriteCmd(name, short string, on bool) *cobra.Command {
	return &cobra.Command{
		Use: name + " ITEM", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			it, err := a.item(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !it.FavoritedKnown {
				return &vinted.Error{Kind: vinted.KindAPIChanged, Endpoint: fmt.Sprintf("GET /items/%d", it.ID),
					Msg: fmt.Sprintf("the item page no longer says whether item %d is in your favorites, and vinted only offers a toggle; nothing was changed", it.ID)}
			}
			if it.Favorited != on {
				if err := a.Client.ToggleFavorite(cmd.Context(), it.ID); err != nil {
					return err
				}
			}
			return a.Printer.Print(actionResult{Action: name + "d", Item: it.ID, OK: true})
		},
	}
}

// openBrowser hands a URL to the desktop's opener; a variable so tests can
// see what would open.
var openBrowser = func(url string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		c = exec.Command("xdg-open", url)
	}
	return c.Start()
}

// user / me

type userView vinted.User

func (v userView) Pretty(w io.Writer, color bool) {
	u := vinted.User(v)
	rows := [][]string{{"id", fmt.Sprint(u.ID)}, {"login", u.Login}}
	if u.Business {
		rows = append(rows, []string{"type", "business"})
	}
	rows = append(rows, []string{"email", verifiedText(u.EmailVerified)})
	if u.PhoneVerified != nil {
		rows = append(rows, []string{"phone", verifiedText(*u.PhoneVerified)})
	}
	if loc := strings.Trim(u.City+", "+u.Country, ", "); loc != "" {
		rows = append(rows, []string{"location", loc})
	}
	rows = append(rows,
		[]string{"rating", fmt.Sprintf("%.1f (%d reviews)", u.Rating, u.Reviews)},
		[]string{"items", fmt.Sprint(u.Items)},
		[]string{"followers", fmt.Sprintf("%d / following %d", u.Followers, u.Following)},
	)
	if !u.LastSeen.IsZero() {
		rows = append(rows, []string{"last seen", relTime(u.LastSeen)})
	}
	if u.URL != "" {
		rows = append(rows, []string{"url", u.URL})
	}
	output.Table(w, color, nil, rows)
}

func verifiedText(ok bool) string {
	if ok {
		return "verified"
	}
	return "not verified"
}

type reviewsView []vinted.Review

func (v reviewsView) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(v))
	for _, r := range v {
		rows = append(rows, []string{r.Date.Format("2006-01-02"), strings.Repeat("★", r.Rating), r.ByLogin, output.Truncate(r.ItemTitle, 30), output.Truncate(r.Comment, 60)})
	}
	output.Table(w, color, []string{"DATE", "RATING", "BY", "ITEM", "COMMENT"}, rows)
}

// pageFlags are the --page/--limit pair of the paged account lists.
type pageFlags struct{ page, limit int }

func (f *pageFlags) bind(cmd *cobra.Command) {
	cmd.Flags().IntVar(&f.page, "page", 1, "result page")
	cmd.Flags().IntVar(&f.limit, "limit", 20, "items per page")
}

func (f pageFlags) check() error {
	if f.page < 1 || f.limit < 1 {
		return output.Usagef("--page and --limit start at 1")
	}
	return nil
}

func pageView(sp vinted.SearchPage) itemsView {
	if sp.Items == nil {
		sp.Items = []vinted.Item{}
	}
	return itemsView{Items: sp.Items, Page: sp.Page, TotalPages: sp.TotalPages, Total: sp.Total}
}

func (a *App) userCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "user",
		Short: "Look at a seller",
		Long:  "Look at a seller. USER is the numeric user id or a /member/{id}-login profile URL.",
	}
	var items pageFlags
	var reviewsPage int
	itemsCmd := &cobra.Command{
		Use: "items USER", Short: "Items in the seller's wardrobe", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := items.check(); err != nil {
				return err
			}
			id, err := vinted.ParseUserRef(args[0])
			if err != nil {
				return err
			}
			sp, err := a.Client.UserItems(cmd.Context(), id, items.page, items.limit)
			if err != nil {
				return err
			}
			return a.Printer.Print(pageView(sp))
		},
	}
	items.bind(itemsCmd)
	reviewsCmd := &cobra.Command{
		Use: "reviews USER", Short: "Reviews received", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := vinted.ParseUserRef(args[0])
			if err != nil {
				return err
			}
			reviews, err := a.Client.Reviews(cmd.Context(), id, reviewsPage)
			if err != nil {
				return err
			}
			if reviews == nil {
				reviews = []vinted.Review{}
			}
			return a.Printer.Print(reviewsView(reviews))
		},
	}
	reviewsCmd.Flags().IntVar(&reviewsPage, "page", 1, "result page")
	cmd.AddCommand(
		&cobra.Command{
			Use: "show USER", Short: "Profile and stats", Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				id, err := vinted.ParseUserRef(args[0])
				if err != nil {
					return err
				}
				u, err := a.Client.User(cmd.Context(), id)
				if err != nil {
					return err
				}
				return a.Printer.Print(userView(u))
			},
		},
		itemsCmd,
		reviewsCmd,
	)
	return cmd
}

func (a *App) meCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "me", Short: "Your own account: profile, items, favorites"}
	var items, favs pageFlags
	itemsCmd := &cobra.Command{
		Use: "items", Short: "Your items", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			if err := items.check(); err != nil {
				return err
			}
			id, err := a.userID(cmd.Context())
			if err != nil {
				return err
			}
			sp, err := a.Client.UserItems(cmd.Context(), id, items.page, items.limit)
			if err != nil {
				return err
			}
			return a.Printer.Print(pageView(sp))
		},
	}
	items.bind(itemsCmd)
	favsCmd := &cobra.Command{
		Use: "favorites", Short: "Items you favorited", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			if err := favs.check(); err != nil {
				return err
			}
			sp, err := a.Client.Favorites(cmd.Context(), favs.page)
			if err != nil {
				return err
			}
			view := pageView(sp)
			if len(view.Items) > favs.limit {
				view.Items = view.Items[:favs.limit]
			}
			return a.Printer.Print(view)
		},
	}
	favs.bind(favsCmd)
	cmd.AddCommand(
		&cobra.Command{
			Use: "show", Short: "Your profile", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := a.requireSession(); err != nil {
					return err
				}
				u, err := a.Client.Me(cmd.Context())
				if err != nil {
					return err
				}
				return a.Printer.Print(userView(u))
			},
		},
		itemsCmd,
		favsCmd,
	)
	return cmd
}
