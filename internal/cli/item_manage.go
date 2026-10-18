package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Microck/vinted-cli/internal/output"
	"github.com/Microck/vinted-cli/internal/vinted"
	"github.com/spf13/cobra"
)

// ownItemCmds are the `item` subcommands that manage the account's own
// items: create (publish), edit, delete, hide and unhide.
func (a *App) ownItemCmds() []*cobra.Command {
	return []*cobra.Command{
		a.itemCreateCmd(),
		a.itemEditCmd(),
		a.itemDeleteCmd(),
		a.itemHideCmd("hide", "Hide one of your items from the catalog and your profile", true),
		a.itemHideCmd("unhide", "Show a hidden item again", false),
	}
}

type uploadedItemView vinted.UploadedItem

func (v uploadedItemView) Pretty(w io.Writer, color bool) {
	l := vinted.UploadedItem(v)
	state := itemState(l.Item)
	if l.Draft {
		state = "draft"
	}
	rows := [][]string{
		{"id", strconv.FormatInt(l.ID, 10)},
		{"title", l.Title},
		{"price", fmt.Sprintf("%.2f %s", l.Price, l.Currency)},
	}
	if state != "" {
		rows = append(rows, []string{"state", state})
	}
	if l.URL != "" {
		rows = append(rows, []string{"url", l.URL})
	}
	output.Table(w, color, nil, rows)
}

// ownedItem resolves ITEM and refuses it unless the active account listed
// it. Vinted's own answer to a write on someone else's item is a bare 404.
func (a *App) ownedItem(ctx context.Context, ref string) (vinted.Item, error) {
	it, err := a.item(ctx, ref)
	if err != nil {
		return vinted.Item{}, err
	}
	me, err := a.userID(ctx)
	if err != nil {
		return vinted.Item{}, err
	}
	if it.SellerID != me {
		return vinted.Item{}, output.Usagef("item %d belongs to another member (%d). Only your own items can be changed", it.ID, it.SellerID)
	}
	return it, nil
}

func (a *App) itemHideCmd(name, short string, hidden bool) *cobra.Command {
	return &cobra.Command{
		Use: name + " ITEM", Short: short, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			it, err := a.ownedItem(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if err := a.Client.SetItemHidden(cmd.Context(), it.ID, hidden); err != nil {
				return err
			}
			return a.Printer.Print(actionResult{Action: name, Item: it.ID, OK: true})
		},
	}
}

func (a *App) itemDeleteCmd() *cobra.Command {
	var yes, draft bool
	cmd := &cobra.Command{
		Use: "delete ITEM", Short: "Delete one of your items (or, with --draft, a draft)", Args: cobra.ExactArgs(1),
		Long: `Delete one of your items for good. With --draft, ITEM is the id of an
unpublished draft (as printed by ` + "`vinted item create --draft`" + `), which is
discarded instead. Asks for confirmation unless --yes is given.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			ctx := cmd.Context()
			if draft {
				id, err := strconv.ParseInt(args[0], 10, 64)
				if err != nil || id <= 0 {
					return output.Usagef("%q is not a draft id", args[0])
				}
				if err := a.confirm(yes, fmt.Sprintf("Discard draft %d? This cannot be undone.", id)); err != nil {
					return err
				}
				if err := a.Client.DeleteDraft(ctx, id); err != nil {
					return err
				}
				return a.Printer.Print(actionResult{Action: "deleted", Item: id, OK: true})
			}
			it, err := a.ownedItem(ctx, args[0])
			if err != nil {
				return err
			}
			if err := a.confirm(yes, fmt.Sprintf("Delete %d (%s) from Vinted? This cannot be undone.", it.ID, it.Title)); err != nil {
				return err
			}
			if err := a.Client.DeleteItem(ctx, it.ID); err != nil {
				return err
			}
			return a.Printer.Print(actionResult{Action: "deleted", Item: it.ID, OK: true})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "do not prompt")
	cmd.Flags().BoolVar(&draft, "draft", false, "ITEM is a draft id")
	return cmd
}

// itemFormFlags are the form fields shared by create and edit.
type itemFormFlags struct {
	title, description, category, condition, size, brand, packageSize, currency string
	price                                                                       float64
	colors, images, attrs                                                       []string
}

func (f *itemFormFlags) bind(cmd *cobra.Command, create bool) {
	verb := "new "
	if create {
		verb = ""
	}
	fl := cmd.Flags()
	fl.StringVar(&f.title, "title", "", verb+"item title")
	fl.StringVar(&f.description, "description", "", verb+"item description")
	fl.Float64Var(&f.price, "price", 0, verb+"price, in the account's currency")
	fl.StringVar(&f.category, "category", "", verb+"category: leaf id or name (see `vinted category list`)")
	fl.StringVar(&f.condition, "condition", "", "condition: name or id (e.g. \"New with tags\", \"Very good\")")
	fl.StringVar(&f.size, "size", "", "size: name or id, for categories that have sizes")
	fl.StringVar(&f.brand, "brand", "", "brand: name or id")
	fl.StringArrayVar(&f.colors, "color", nil, "colour name or id (repeatable, at most 2)")
	fl.StringVar(&f.packageSize, "package-size", "", "shipping size: small, medium, large or id (default: smallest offered)")
	fl.StringArrayVar(&f.attrs, "attr", nil, "other category attribute code=value, value as name or id (repeatable)")
	if create {
		fl.StringArrayVar(&f.images, "image", nil, "image file to upload (repeatable, at least one)")
		fl.StringVar(&f.currency, "currency", "", "currency code (default: the account's)")
	} else {
		fl.StringArrayVar(&f.images, "image", nil, "replacement image files (repeatable); replaces every photo")
	}
}

// itemCreateCmd publishes a new item from the terminal. Required fields
// missing on the command line are a usage error (exit 2) before anything is
// uploaded; so are values the category does not offer.
func (a *App) itemCreateCmd() *cobra.Command {
	var f itemFormFlags
	var draft bool
	cmd := &cobra.Command{
		Use:     "create",
		Aliases: []string{"publish"},
		Short:   "Publish a new item with one or more images",
		Long: `Publish a new item as the active profile, or save it as a draft with
--draft (visible only to you; finish it in the web app).

--category takes a leaf category id or an unambiguous name. --condition,
--size and --attr values are checked against what Vinted offers for that
category, by name or id, before anything is uploaded. Attributes the category
requires (condition, usually size) must be given. Publishing (not --draft)
also needs --brand and --color for most categories; Vinted names any missing
field in its error (exit 2).

Examples:
  vinted item create --title "Levi's 501" --description "Worn twice" --price 25 --category 1840 --condition "Very good" --size "W32" --brand "Levi's" --color Blue --image front.jpg --image back.jpg
  vinted item create --title "Lego 42083" --description "Complete" --price 120 --category "Lego" --condition "New with tags" --image box.jpg --draft`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			var missing []string
			if f.title == "" {
				missing = append(missing, "--title")
			}
			if f.description == "" {
				missing = append(missing, "--description")
			}
			if !cmd.Flags().Changed("price") {
				missing = append(missing, "--price")
			}
			if f.category == "" {
				missing = append(missing, "--category")
			}
			if f.condition == "" {
				missing = append(missing, "--condition")
			}
			if len(f.images) == 0 {
				missing = append(missing, "--image FILE")
			}
			if len(missing) > 0 {
				return output.Usagef("missing required %s", strings.Join(missing, ", "))
			}
			if f.price <= 0 {
				return output.Usagef("--price must be positive")
			}
			files, err := loadItemImages(f.images)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			catalogID, err := a.uploadCategory(ctx, f.category)
			if err != nil {
				return err
			}
			form, err := a.resolveForm(ctx, catalogID, f, true)
			if err != nil {
				return err
			}
			currency := strings.ToUpper(f.currency)
			if currency == "" {
				if currency, err = a.Client.AccountCurrency(ctx); err != nil {
					return err
				}
				if currency == "" {
					return output.Usagef("could not tell the account's currency; pass --currency")
				}
			}
			l, err := a.Client.CreateItem(ctx, vinted.ItemInput{
				Title: f.title, Description: f.description, Price: f.price, Currency: currency,
				CatalogID: catalogID, BrandID: form.brandID, Brand: form.brand, ColorIDs: form.colors,
				PackageSizeID: form.packageSize, Attributes: form.attrs, Photos: files,
			}, draft)
			if err != nil {
				return err
			}
			return a.Printer.Print(uploadedItemView(l))
		},
	}
	f.bind(cmd, true)
	cmd.Flags().BoolVar(&draft, "draft", false, "save as a draft instead of publishing")
	return cmd
}

// itemEditCmd changes fields on one of the account's own items. Only the
// flags passed change; everything else stays as it was.
func (a *App) itemEditCmd() *cobra.Command {
	var f itemFormFlags
	cmd := &cobra.Command{
		Use:   "edit ITEM",
		Short: "Change title, description, price, attributes or images on your item",
		Long: `Change fields on one of your items, as saving the web app's edit form
does. Only the flags given change. --condition, --size and --attr are checked
against the item's category (or the new --category).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.requireSession(); err != nil {
				return err
			}
			fl := cmd.Flags()
			changed := false
			for _, name := range []string{"title", "description", "price", "category", "condition", "size", "brand", "color", "package-size", "attr", "image"} {
				changed = changed || fl.Changed(name)
			}
			if !changed {
				return output.Usagef("nothing to change. Pass at least one of --title, --description, --price, --category, --condition, --size, --brand, --color, --package-size, --attr, --image")
			}
			if fl.Changed("price") && f.price <= 0 {
				return output.Usagef("--price must be positive")
			}
			var files []vinted.UploadImage
			if len(f.images) > 0 {
				var err error
				if files, err = loadItemImages(f.images); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			it, err := a.ownedItem(ctx, args[0])
			if err != nil {
				return err
			}
			e := vinted.ItemEdit{Photos: files}
			if fl.Changed("title") {
				e.Title = &f.title
			}
			if fl.Changed("description") {
				e.Description = &f.description
			}
			if fl.Changed("price") {
				e.Price = &f.price
			}
			catalogID := it.CategoryID
			if fl.Changed("category") {
				if catalogID, err = a.uploadCategory(ctx, f.category); err != nil {
					return err
				}
				e.CatalogID = &catalogID
			}
			needsForm := fl.Changed("condition") || fl.Changed("size") || fl.Changed("brand") || fl.Changed("color") || fl.Changed("package-size") || fl.Changed("attr")
			if needsForm {
				if catalogID == 0 {
					return output.Usagef("could not tell item %d's category; pass --category too", it.ID)
				}
				form, err := a.resolveForm(ctx, catalogID, f, false)
				if err != nil {
					return err
				}
				e.Attributes = form.attrs
				if fl.Changed("brand") {
					e.BrandID, e.Brand = &form.brandID, &form.brand
				}
				if fl.Changed("color") {
					e.ColorIDs = form.colors
				}
				if fl.Changed("package-size") {
					e.PackageSizeID = &form.packageSize
				}
			}
			l, err := a.Client.EditItem(ctx, it.ID, e)
			if err != nil {
				return err
			}
			return a.Printer.Print(uploadedItemView(l))
		},
	}
	f.bind(cmd, false)
	return cmd
}

// resolvedForm is the item flags turned into the ids the upload API takes.
type resolvedForm struct {
	attrs       []vinted.AttributeValue
	brandID     int
	brand       string
	colors      []int
	packageSize int
}

// resolveForm validates the attribute, brand, colour and package flags
// against what Vinted offers for the category. With create, required
// attributes must be present and the package size defaults to the smallest.
func (a *App) resolveForm(ctx context.Context, catalogID int, f itemFormFlags, create bool) (resolvedForm, error) {
	var out resolvedForm
	wanted := map[string][]string{}
	var order []string
	add := func(code, value string) {
		if _, ok := wanted[code]; !ok {
			order = append(order, code)
		}
		wanted[code] = append(wanted[code], value)
	}
	if f.condition != "" {
		add("condition", f.condition)
	}
	if f.size != "" {
		add("size", f.size)
	}
	for _, p := range f.attrs {
		k, v, ok := strings.Cut(p, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok || k == "" || strings.TrimSpace(v) == "" {
			return out, output.Usagef("bad --attr %q. Use code=value", p)
		}
		switch k {
		case "condition", "size", "brand", "color", "category":
			return out, output.Usagef("set %s with --%s, not --attr", k, k)
		}
		add(k, strings.TrimSpace(v))
	}
	if len(wanted) > 0 || create {
		defs, err := a.Client.UploadAttributes(ctx, catalogID)
		if err != nil {
			return out, err
		}
		byCode := map[string]vinted.UploadAttribute{}
		codes := make([]string, 0, len(defs))
		for _, d := range defs {
			// An attribute vinted sent in an unknown shape has no code to match.
			if d.Code == "" {
				continue
			}
			byCode[d.Code] = d
			codes = append(codes, d.Code)
		}
		for _, code := range order {
			def, ok := byCode[code]
			if !ok {
				return out, output.Usagef("category %d has no %q attribute. Its attributes: %s", catalogID, code, strings.Join(codes, ", "))
			}
			values := wanted[code]
			if !def.Multi && len(values) > 1 {
				return out, output.Usagef("%s takes one value, got %d", code, len(values))
			}
			if def.Limit > 0 && len(values) > def.Limit {
				return out, output.Usagef("%s takes at most %d values", code, def.Limit)
			}
			ids := make([]int, 0, len(values))
			for _, v := range values {
				id, err := pickOption(def.Options, v, code)
				if err != nil {
					return out, err
				}
				ids = append(ids, id)
			}
			out.attrs = append(out.attrs, vinted.AttributeValue{Code: code, IDs: ids})
		}
		if create {
			var missing []string
			for _, d := range defs {
				if _, ok := wanted[d.Code]; d.Required && !ok {
					missing = append(missing, attrFlag(d.Code))
				}
			}
			if len(missing) > 0 {
				return out, output.Usagef("category %d requires %s", catalogID, strings.Join(missing, ", "))
			}
		}
	}
	if f.brand != "" {
		if id, err := strconv.Atoi(f.brand); err == nil && id > 0 {
			out.brandID = id
		} else {
			brands, err := a.Client.UploadBrands(ctx, f.brand, catalogID)
			if err != nil {
				return out, err
			}
			found := false
			var titles []string
			for _, b := range brands {
				if strings.EqualFold(b.Title, f.brand) {
					out.brandID, out.brand, found = b.ID, b.Title, true
					break
				}
				titles = append(titles, b.Title)
			}
			if !found {
				hint := ""
				if len(titles) > 0 {
					hint = ". Close matches: " + strings.Join(titles[:min(len(titles), 8)], ", ")
				}
				return out, output.Usagef("no brand named %q%s", f.brand, hint)
			}
		}
	}
	if len(f.colors) > 0 {
		if len(f.colors) > 2 {
			return out, output.Usagef("an item takes at most 2 colours")
		}
		colors, err := a.Client.UploadColors(ctx)
		if err != nil {
			return out, err
		}
		for _, c := range f.colors {
			id, err := pickOption(colors, c, "color")
			if err != nil {
				return out, err
			}
			out.colors = append(out.colors, id)
		}
	}
	if f.packageSize != "" || create {
		sizes, err := a.Client.PackageSizes(ctx, catalogID)
		if err != nil {
			return out, err
		}
		if len(sizes) == 0 {
			if f.packageSize != "" {
				return out, output.Usagef("category %d offers no package sizes", catalogID)
			}
			return out, nil
		}
		if f.packageSize == "" {
			out.packageSize = sizes[0].ID
			return out, nil
		}
		opts := make([]vinted.AttributeOption, 0, len(sizes)*2)
		for _, s := range sizes {
			opts = append(opts, vinted.AttributeOption{ID: s.ID, Title: s.Title}, vinted.AttributeOption{ID: s.ID, Title: s.Code})
		}
		if out.packageSize, err = pickOption(opts, f.packageSize, "package-size"); err != nil {
			return out, err
		}
	}
	return out, nil
}

func attrFlag(code string) string {
	switch code {
	case "condition", "size":
		return "--" + code
	}
	return "--attr " + code + "=VALUE"
}

// pickOption matches value against options by id or by title, ignoring case.
func pickOption(opts []vinted.AttributeOption, value, what string) (int, error) {
	value = strings.TrimSpace(value)
	if id, err := strconv.Atoi(value); err == nil {
		for _, o := range opts {
			if o.ID == id {
				return id, nil
			}
		}
	}
	for _, o := range opts {
		if strings.EqualFold(o.Title, value) {
			return o.ID, nil
		}
	}
	names := make([]string, 0, len(opts))
	seen := map[string]bool{}
	for _, o := range opts {
		if !seen[o.Title] {
			seen[o.Title] = true
			names = append(names, o.Title)
		}
	}
	if len(names) > 25 {
		names = append(names[:25], "…")
	}
	return 0, output.Usagef("%q is not a valid %s here. Valid: %s", value, what, strings.Join(names, ", "))
}

// uploadCategory resolves --category to a leaf of the upload category tree:
// a numeric id, or a name that matches exactly one leaf (case-insensitive).
func (a *App) uploadCategory(ctx context.Context, ref string) (int, error) {
	cats, err := a.Client.Categories(ctx)
	if err != nil {
		return 0, err
	}
	type leaf struct {
		id          int
		title, path string
	}
	var leaves []leaf
	var all []leaf
	var walk func(cs []vinted.Category, prefix string)
	walk = func(cs []vinted.Category, prefix string) {
		for _, c := range cs {
			if c.ID == 0 {
				// An entry Vinted sent in a shape the client cannot read
				// (its raw JSON is in Unparsed) names no category.
				continue
			}
			p := c.Title
			if prefix != "" {
				p = prefix + " > " + c.Title
			}
			all = append(all, leaf{c.ID, c.Title, p})
			if len(c.Children) == 0 {
				leaves = append(leaves, leaf{c.ID, c.Title, p})
			}
			walk(c.Children, p)
		}
	}
	walk(cats, "")
	ref = strings.TrimSpace(ref)
	if id, err := strconv.Atoi(ref); err == nil {
		for _, l := range leaves {
			if l.id == id {
				return id, nil
			}
		}
		for _, c := range all {
			if c.id == id {
				return 0, output.Usagef("category %d (%s) has subcategories; pick a leaf. See `vinted category list`", id, c.path)
			}
		}
		return 0, output.Usagef("category %d does not exist. See `vinted category list`", id)
	}
	var matches []leaf
	for _, l := range leaves {
		if strings.EqualFold(l.title, ref) || strings.EqualFold(l.path, ref) {
			matches = append(matches, l)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].id, nil
	case 0:
		for _, c := range all {
			if strings.EqualFold(c.title, ref) || strings.EqualFold(c.path, ref) {
				return 0, output.Usagef("category %q (%d, %s) has subcategories; pick a leaf. See `vinted category list`", ref, c.id, c.path)
			}
		}
		return 0, output.Usagef("no leaf category named %q. See `vinted category list`", ref)
	}
	opts := make([]string, 0, len(matches))
	for _, m := range matches {
		opts = append(opts, fmt.Sprintf("%d (%s)", m.id, m.path))
	}
	return 0, output.Usagef("category %q is ambiguous: %s. Pass the id", ref, strings.Join(opts, "; "))
}

// loadItemImages reads image files, refusing non-images and files over
// vinted.MaxPhotoBytes before any upload.
func loadItemImages(paths []string) ([]vinted.UploadImage, error) {
	out := make([]vinted.UploadImage, 0, len(paths))
	for _, p := range paths {
		raw, err := readImageFile(p)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, output.Usagef("image %s is empty", p)
		}
		ct := http.DetectContentType(raw[:min(512, len(raw))])
		if !strings.HasPrefix(ct, "image/") {
			return nil, output.Usagef("image %s is not an image (%s)", p, ct)
		}
		out = append(out, vinted.UploadImage{Name: filepath.Base(p), Data: raw, ContentType: ct})
	}
	return out, nil
}

// readImageFile reads one image file, refusing it when larger than
// vinted.MaxPhotoBytes: the size is checked before reading, and the read is
// capped in case the file grows meanwhile or is not a regular file.
func readImageFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, output.Usagef("cannot read image %s: %v", p, err)
	}
	defer f.Close()
	tooBig := func() error {
		return output.Usagef("image %s is larger than %d MiB", p, vinted.MaxPhotoBytes>>20)
	}
	if st, err := f.Stat(); err != nil {
		return nil, output.Usagef("cannot read image %s: %v", p, err)
	} else if st.Size() > vinted.MaxPhotoBytes {
		return nil, tooBig()
	}
	raw, err := io.ReadAll(io.LimitReader(f, vinted.MaxPhotoBytes+1))
	if err != nil {
		return nil, output.Usagef("cannot read image %s: %v", p, err)
	}
	if len(raw) > vinted.MaxPhotoBytes {
		return nil, tooBig()
	}
	return raw, nil
}
