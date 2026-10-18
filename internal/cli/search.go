package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
)

// searchFlags are shared by `search`, `search filters` and `watch add search`.
type searchFlags struct {
	priceFrom  float64
	priceTo    float64
	currency   string
	order      string
	catalogs   []int
	brands     []int
	sizes      []int
	conditions []string
	colors     []int
	materials  []int
	// byCode holds the attrFlags values, keyed by filter code.
	byCode map[string]*[]int
	attrs  []string
	filter []string
}

// conditionIDs maps condition names to the status filter's option ids, as
// `vinted search filters` lists them (not_fully_functional only exists in
// electronics). Raw ids pass through for anything newer.
var conditionIDs = map[string]int{"new_with_tags": 6, "new": 1, "new_without_tags": 1, "very_good": 2, "good": 3, "satisfactory": 4, "not_fully_functional": 7}

var conditionNames = []string{"new_with_tags", "new_without_tags", "new", "very_good", "good", "satisfactory", "not_fully_functional"}

var orders = []string{"relevance", "newest_first", "price_low_to_high", "price_high_to_low"}

// attrFlags are the category filters the catalog offers beyond the common
// ones, each with its own flag. The codes are the filter service's (`vinted
// search filters --catalog ID`); a code becomes the query key CODE_ids[].
// Codes not listed here go through --attr CODE=ID.
var attrFlags = []struct{ flag, code, usage string }{
	{"pattern", "patterns", "pattern `id`, repeatable (clothing)"},
	{"video-game-rating", "video_game_rating", "content rating `id`, repeatable (video games)"},
	{"platform", "video_game_platform", "video game platform `id`, repeatable (video games)"},
	{"model", "brand_collection", "model `id`, repeatable (phones and other electronics)"},
	{"storage", "internal_memory_capacity", "storage capacity `id`, repeatable (phones, tablets)"},
	{"sim-lock", "sim_lock", "SIM lock `id`, repeatable (phones)"},
	{"language", "language_book", "book language `id`, repeatable (books)"},
}

// filterFlags names the search flag of each filter code with one; the rest
// go through --attr CODE=ID.
var filterFlags = func() map[string]string {
	m := map[string]string{
		"catalog": "--catalog", "brand": "--brand", "size": "--size", "status": "--condition", "color": "--color",
		"material": "--material", "price": "--price-from/--price-to/--currency",
	}
	for _, a := range attrFlags {
		m[a.code] = "--" + a.flag
	}
	return m
}()

func filterFlag(code string) string {
	if f, ok := filterFlags[code]; ok {
		return f
	}
	return "--attr " + code + "=ID"
}

// bindSearchFlags defines the search flags on cmd and returns them as their
// own set, so callers can ask which of exactly these flags were given.
func (a *App) bindSearchFlags(cmd *cobra.Command, f *searchFlags) *pflag.FlagSet {
	fl := pflag.NewFlagSet("search", pflag.ContinueOnError)
	fl.Float64Var(&f.priceFrom, "price-from", 0, "minimum price")
	fl.Float64Var(&f.priceTo, "price-to", 0, "maximum price")
	fl.StringVar(&f.currency, "currency", "", "`code` of the --price-from/--price-to currency (EUR, USD...; default the site's)")
	fl.StringVar(&f.order, "order", "", "order: "+strings.Join(orders, ", "))
	fl.IntSliceVar(&f.catalogs, "catalog", nil, "category `id`, repeatable (ids from vinted category list)")
	fl.IntSliceVar(&f.brands, "brand", nil, "brand `id`, repeatable (ids from vinted brand search)")
	fl.IntSliceVar(&f.sizes, "size", nil, "size `id`, repeatable (ids from vinted search filters --catalog ID)")
	fl.StringSliceVar(&f.conditions, "condition", nil, "item condition, repeatable: "+strings.Join(conditionNames, ", ")+", or a status id")
	fl.IntSliceVar(&f.colors, "color", nil, "colour `id`, repeatable (ids from vinted search filters)")
	fl.IntSliceVar(&f.materials, "material", nil, "material `id`, repeatable (ids from vinted search filters)")
	f.byCode = map[string]*[]int{}
	for _, af := range attrFlags {
		ids := new([]int)
		f.byCode[af.code] = ids
		fl.IntSliceVar(ids, af.flag, nil, af.usage)
	}
	fl.StringArrayVar(&f.attrs, "attr", nil, "any other filter as `CODE=ID[,ID]`, repeatable (codes and ids from vinted search filters)")
	fl.StringArrayVar(&f.filter, "filter", nil, "any other catalog query parameter as `key=value`, repeatable, sent verbatim")
	cmd.Flags().AddFlagSet(fl)
	_ = cmd.RegisterFlagCompletionFunc("condition", cobra.FixedCompletions(conditionNames, cobra.ShellCompDirectiveNoFileComp))
	_ = cmd.RegisterFlagCompletionFunc("order", cobra.FixedCompletions(orders, cobra.ShellCompDirectiveNoFileComp))
	return fl
}

// toParams validates the flags into catalog search parameters.
func toParams(keywords []string, f searchFlags) (vinted.SearchParams, error) {
	p := vinted.SearchParams{
		Text: strings.Join(keywords, " "), CatalogIDs: f.catalogs, BrandIDs: f.brands,
		SizeIDs: f.sizes, ColorIDs: f.colors, MaterialIDs: f.materials,
		PriceFrom: f.priceFrom, PriceTo: f.priceTo, Currency: strings.ToUpper(f.currency),
	}
	if f.priceFrom < 0 || f.priceTo < 0 {
		return p, output.Usagef("prices cannot be negative")
	}
	if f.priceTo > 0 && f.priceFrom > f.priceTo {
		return p, output.Usagef("--price-from %v is above --price-to %v", f.priceFrom, f.priceTo)
	}
	for _, c := range f.conditions {
		id, ok := conditionIDs[c]
		if !ok {
			n, err := strconv.Atoi(c)
			if err != nil || n <= 0 {
				return p, output.Usagef("--condition %q is not one of %s, or a status id", c, strings.Join(conditionNames, ", "))
			}
			id = n
		}
		p.StatusIDs = append(p.StatusIDs, id)
	}
	if f.order != "" {
		ok := false
		for _, o := range orders {
			ok = ok || o == f.order
		}
		if !ok {
			return p, output.Usagef("--order must be one of %s", strings.Join(orders, ", "))
		}
		p.Order = f.order
	}
	addAttr := func(code string, id int) {
		if p.Attributes == nil {
			p.Attributes = map[string][]int{}
		}
		p.Attributes[code] = append(p.Attributes[code], id)
	}
	for _, af := range attrFlags {
		if ids := f.byCode[af.code]; ids != nil {
			for _, id := range *ids {
				if id <= 0 {
					return p, output.Usagef("--%s %d is not an option id", af.flag, id)
				}
				addAttr(af.code, id)
			}
		}
	}
	for _, kv := range f.attrs {
		code, list, ok := strings.Cut(kv, "=")
		code = strings.TrimSuffix(strings.TrimSpace(code), "_ids")
		if !ok || code == "" || list == "" {
			return p, output.Usagef("--attr %q is not CODE=ID[,ID] (codes and ids from `vinted search filters`)", kv)
		}
		if flag, has := filterFlags[code]; has {
			return p, output.Usagef("--attr %s: use %s for this filter", code, flag)
		}
		for _, s := range strings.Split(list, ",") {
			id, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || id <= 0 {
				return p, output.Usagef("--attr %q: %q is not an option id", kv, s)
			}
			addAttr(code, id)
		}
	}
	for _, kv := range f.filter {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return p, output.Usagef("--filter %q is not key=value", kv)
		}
		if p.Extra == nil {
			p.Extra = url.Values{}
		}
		p.Extra.Add(k, v)
	}
	return p, nil
}

// describeParams is the one-line form of a search, for watch listings.
func describeParams(p vinted.SearchParams) string {
	var parts []string
	if p.Text != "" {
		parts = append(parts, strconv.Quote(p.Text))
	}
	ids := func(name string, v []int) {
		if len(v) == 0 {
			return
		}
		s := make([]string, len(v))
		for i, n := range v {
			s[i] = strconv.Itoa(n)
		}
		parts = append(parts, name+"="+strings.Join(s, ","))
	}
	ids("catalog", p.CatalogIDs)
	ids("brand", p.BrandIDs)
	ids("size", p.SizeIDs)
	ids("status", p.StatusIDs)
	ids("color", p.ColorIDs)
	ids("material", p.MaterialIDs)
	codes := make([]string, 0, len(p.Attributes))
	for code := range p.Attributes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		ids(code, p.Attributes[code])
	}
	if p.PriceFrom > 0 || p.PriceTo > 0 {
		price := fmt.Sprintf("price=%v-%v", p.PriceFrom, p.PriceTo)
		if p.Currency != "" {
			price += " " + p.Currency
		}
		parts = append(parts, price)
	}
	if p.Order != "" {
		parts = append(parts, "order="+p.Order)
	}
	keys := make([]string, 0, len(p.Extra))
	for k := range p.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+strings.Join(p.Extra[k], ","))
	}
	return strings.Join(parts, " ")
}

type itemsView struct {
	Items      []vinted.Item `json:"items"`
	Page       int           `json:"page,omitempty"`
	TotalPages int           `json:"total_pages,omitempty"`
	Total      int           `json:"total,omitempty"`
	// Unparsed holds the page parts vinted sent in a shape the cli does not
	// know; with several pages fetched, keys start with "pages.<n>.".
	Unparsed map[string]json.RawMessage `json:"unparsed,omitempty"`
}

func (v itemsView) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(v.Items))
	for _, it := range v.Items {
		flags := ""
		if it.Reserved {
			flags += "R"
		}
		if it.Sold {
			flags += "S"
		}
		rows = append(rows, []string{fmt.Sprint(it.ID), fmt.Sprintf("%.2f %s", it.Price, it.Currency), flags, output.Truncate(it.Title, 40), output.Truncate(it.Brand, 14), output.Truncate(it.Size, 8), output.Dim(it.URL, color)})
	}
	output.Table(w, color, []string{"ID", "PRICE", "", "TITLE", "BRAND", "SIZE", "URL"}, rows)
	if v.Page > 0 && v.Page < v.TotalPages {
		fmt.Fprintln(w, output.Dim(fmt.Sprintf("page %d of %d (%d items); more: --page %d", v.Page, v.TotalPages, v.Total, v.Page+1), color))
	}
}

func (a *App) searchCmd() *cobra.Command {
	var f searchFlags
	var limit, pages, page int
	cmd := &cobra.Command{
		Use:   "search [text...]",
		Short: "Search the catalog",
		Long: `Search the Vinted catalog of the profile's site.

Common filters are flags; ids for --catalog and --brand come from
` + "`vinted category list`" + ` and ` + "`vinted brand search`" + `. Any other catalog query
parameter goes through --filter key=value verbatim. No login is needed.

Examples:
  vinted search "levis 501" --price-to 30 --order newest_first --format pretty
  vinted search --catalog 1206 --brand 53 --condition very_good --condition good
  vinted search nike --pages 3 --limit 100 --format jsonl
  vinted search nike --page 2`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := toParams(args, f)
			if err != nil {
				return err
			}
			if page < 1 || pages < 1 {
				return output.Usagef("--page and --pages start at 1")
			}
			if pages > maxSearchPages {
				return output.Usagef("--pages is at most %d: every page is one more request to vinted, and bursts get the session blocked. Narrow the search or use --page to continue later", maxSearchPages)
			}
			view := itemsView{Items: []vinted.Item{}}
			for p.Page = page; p.Page < page+pages; p.Page++ {
				sp, err := a.Client.Search(cmd.Context(), p)
				if err != nil {
					return err
				}
				view.Items = append(view.Items, sp.Items...)
				for k, raw := range sp.Unparsed {
					if pages > 1 {
						k = "pages." + strconv.Itoa(p.Page) + "." + k
					}
					if view.Unparsed == nil {
						view.Unparsed = map[string]json.RawMessage{}
					}
					view.Unparsed[k] = raw
				}
				view.Page, view.TotalPages, view.Total = sp.Page, sp.TotalPages, sp.Total
				if len(sp.Items) == 0 || sp.Page >= sp.TotalPages || (limit > 0 && len(view.Items) >= limit) {
					break
				}
			}
			if limit > 0 && len(view.Items) > limit {
				view.Items = view.Items[:limit]
			}
			return a.Printer.Print(view)
		},
	}
	a.bindSearchFlags(cmd, &f)
	cmd.Flags().IntVar(&limit, "limit", 0, "stop after this many items (0 = whole pages)")
	cmd.Flags().IntVar(&page, "page", 1, "first result page")
	cmd.Flags().IntVar(&pages, "pages", 1, fmt.Sprintf("how many result pages to fetch (max %d)", maxSearchPages))
	cmd.AddCommand(a.searchFiltersCmd())
	return cmd
}

type filtersView []vinted.Filter

func (v filtersView) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(v))
	for _, f := range v {
		// A filter vinted sent in an unknown shape has only its raw JSON,
		// which the json output carries.
		if f.Code == "" {
			continue
		}
		pick := "one"
		if f.Multi {
			pick = "many"
		}
		rows = append(rows, []string{f.Code, f.Title, f.Type, pick, filterFlag(f.Code), strings.Join(f.Params, " ")})
	}
	output.Table(w, color, []string{"CODE", "TITLE", "TYPE", "PICK", "FLAG", "PARAMS"}, rows)
	for _, f := range v {
		if len(f.Options) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%s)\n", f.Title, f.Code)
		var opts [][]string
		var walk func(os []vinted.FilterOption, depth int)
		walk = func(os []vinted.FilterOption, depth int) {
			for _, o := range os {
				id, title := fmt.Sprint(o.ID), strings.Repeat("  ", depth)+o.Title
				if !o.Selectable() {
					id, title = "", output.Dim(title, color)
				}
				var extra []string
				if o.Hex != "" {
					extra = append(extra, o.Hex)
				}
				if o.Count > 0 {
					extra = append(extra, fmt.Sprintf("%d items", o.Count))
				}
				opts = append(opts, []string{id, title, strings.Join(extra, " ")})
				walk(o.Options, depth+1)
			}
		}
		walk(f.Options, 0)
		output.Table(w, color, []string{"ID", "OPTION", ""}, opts)
	}
}

func (a *App) searchFiltersCmd() *cobra.Command {
	var f searchFlags
	var codes []string
	cmd := &cobra.Command{
		Use:   "filters [text...]",
		Short: "List the catalog filters with their option ids",
		Long: `List the filters Vinted offers for a search, as the site's filter bar shows
them: each filter's code, whether several options can be picked, the search
flag that sets it (--attr CODE=ID for filters without their own flag), and
every option id with its title. Options depend on the category, so pass
--catalog for the category you search in. Brand lists only popular brands;
find any other with ` + "`vinted brand search`" + `. The other search flags narrow the
options the way a selection on the site does.

The filter list is one request to Vinted and each filter's options one more,
as on the site; --code loads only the options of the filters named.

Examples:
  vinted search filters --format pretty
  vinted search filters --catalog 1845 --code size --format pretty
  vinted search filters --catalog 1845 | jq '.[] | {code, params}'`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := toParams(args, f)
			if err != nil {
				return err
			}
			filters, err := a.Client.Filters(cmd.Context(), p, codes...)
			if err != nil {
				return err
			}
			return a.Printer.Print(filtersView(filters))
		},
	}
	a.bindSearchFlags(cmd, &f)
	cmd.Flags().StringSliceVar(&codes, "code", nil, "load options only for these filter `codes` (size, status, color...), repeatable")
	return cmd
}

type categoriesView []vinted.Category

func (v categoriesView) Pretty(w io.Writer, color bool) {
	var rows [][]string
	var walk func(cs []vinted.Category, depth int)
	walk = func(cs []vinted.Category, depth int) {
		for _, c := range cs {
			rows = append(rows, []string{fmt.Sprint(c.ID), strings.Repeat("  ", depth) + c.Title})
			walk(c.Children, depth+1)
		}
	}
	walk(v, 0)
	output.Table(w, color, []string{"ID", "CATEGORY"}, rows)
}

func (a *App) categoryCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "category", Short: "Browse Vinted's category tree"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List categories and subcategories with their ids", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cats, err := a.Client.Categories(cmd.Context())
			if err != nil {
				return err
			}
			return a.Printer.Print(categoriesView(cats))
		},
	})
	return cmd
}

type brandsView []vinted.Brand

func (v brandsView) Pretty(w io.Writer, color bool) {
	rows := make([][]string, 0, len(v))
	for _, b := range v {
		rows = append(rows, []string{fmt.Sprint(b.ID), b.Title, fmt.Sprint(b.Items)})
	}
	output.Table(w, color, []string{"ID", "BRAND", "ITEMS"}, rows)
}

func (a *App) brandCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "brand", Short: "Find brand ids for --brand"}
	cmd.AddCommand(&cobra.Command{
		Use: "search TEXT", Short: "Brands whose name matches TEXT", Args: cobra.MinimumNArgs(1),
		Example: "  vinted brand search levis --format pretty",
		RunE: func(cmd *cobra.Command, args []string) error {
			brands, err := a.Client.Brands(cmd.Context(), strings.Join(args, " "))
			if err != nil {
				return err
			}
			if brands == nil {
				brands = []vinted.Brand{}
			}
			return a.Printer.Print(brandsView(brands))
		},
	})
	return cmd
}
