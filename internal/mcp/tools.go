package mcp

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Tool is one MCP tool: its JSON Schema, and the CLI argv it turns into.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema schema `json:"inputSchema"`

	// build maps validated input to the command line. Only the flags the input
	// actually carries are emitted, so the CLI's own defaults still apply.
	build func(b *argv) `json:"-"`
}

// argv builds one command line and collects the first input error.
func (t Tool) argv(in map[string]any) ([]string, error) {
	if in == nil {
		in = map[string]any{}
	}
	if err := t.InputSchema.validate(in); err != nil {
		return nil, err
	}
	b := &argv{in: in}
	t.build(b)
	if b.err != nil {
		return nil, b.err
	}
	// Positionals go last behind --, so search text or an id that starts with a
	// dash is never read as a flag.
	if len(b.rest) > 0 {
		b.out = append(b.out, "--")
		b.out = append(b.out, b.rest...)
	}
	return b.out, nil
}

// withheld names the commands that will not be tools, with the reason an agent
// gets when it asks for one. Actions that reach another person or change the
// account stay in the hands of the person at the terminal.
var withheld = []struct{ name, why string }{
	{"chat_send", "sending a message reaches a real person; run `vinted chat send` yourself"},
	{"chat_start", "messaging a seller reaches a real person; run `vinted chat start` yourself"},
	{"chat_open", "an interactive chat reaches a real person; run `vinted chat open` yourself"},
	{"chat_offer", "an offer reaches a real person and can commit to a price; run `vinted chat offer` yourself"},
	{"chat_reserve", "reserving promises the item to a buyer; run `vinted chat reserve` yourself"},
	{"item_favorite", "favoriting changes the account and notifies the seller; run `vinted item favorite` yourself"},
	{"item_unfavorite", "favoriting changes the account; run `vinted item unfavorite` yourself"},
	{"item_create", "publishing an item changes the account in public; run `vinted item create` yourself"},
	{"item_edit", "editing an item changes it in public; run `vinted item edit` yourself"},
	{"item_delete", "deleting an item cannot be undone; run `vinted item delete` yourself"},
	{"item_hide", "hiding an item changes the account; run `vinted item hide` yourself"},
	{"item_unhide", "showing an item changes the account; run `vinted item unhide` yourself"},
}

func listTools() []Tool {
	return []Tool{
		searchTool, searchFiltersTool, itemShowTool, userShowTool, userItemsTool, userReviewsTool,
		categoryListTool, brandSearchTool, meShowTool, meFavoritesTool,
		alertListTool, chatListTool, chatShowTool, watchCheckTool,
	}
}

func lookupTool(name string) (Tool, bool) {
	for _, t := range listTools() {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// Tools

// searchFilterProps are the filters `vinted search` and `vinted search
// filters` share, one property per flag, named after the flag.
func searchFilterProps() props {
	return props{
		"text":              text("what to search for"),
		"price_from":        number("minimum price"),
		"price_to":          number("maximum price"),
		"currency":          text("currency code of price_from/price_to (EUR, USD...); default the site's"),
		"order":             oneOf("result order", "relevance", "newest_first", "price_low_to_high", "price_high_to_low"),
		"catalog":           list(text("category id, from category_list")),
		"brand":             list(text("brand id, from brand_search")),
		"size":              list(text("size id, from search_filters")),
		"condition":         list(text("new_with_tags, new_without_tags, new, very_good, good, satisfactory, not_fully_functional, or a status id")),
		"color":             list(text("colour id, from search_filters")),
		"material":          list(text("material id, from search_filters")),
		"pattern":           list(text("pattern id, from search_filters (clothing)")),
		"video_game_rating": list(text("content rating id, from search_filters (video games)")),
		"platform":          list(text("video game platform id, from search_filters")),
		"model":             list(text("model id, from search_filters (phones and other electronics)")),
		"storage":           list(text("storage capacity id, from search_filters (phones, tablets)")),
		"sim_lock":          list(text("SIM lock id, from search_filters (phones)")),
		"language":          list(text("book language id, from search_filters (books)")),
		"attr":              list(text("any other filter as CODE=ID[,ID], codes and ids from search_filters")),
		"filter":            list(text("any other catalog query parameter as key=value, sent verbatim; repeat a key for several values")),
	}
}

func buildSearchFilters(b *argv) {
	b.number("price_from", "--price-from")
	b.number("price_to", "--price-to")
	b.text("currency", "--currency")
	b.text("order", "--order")
	for _, f := range [][2]string{
		{"catalog", "--catalog"}, {"brand", "--brand"}, {"size", "--size"}, {"condition", "--condition"},
		{"color", "--color"}, {"material", "--material"}, {"pattern", "--pattern"},
		{"video_game_rating", "--video-game-rating"}, {"platform", "--platform"}, {"model", "--model"},
		{"storage", "--storage"}, {"sim_lock", "--sim-lock"}, {"language", "--language"},
		{"attr", "--attr"}, {"filter", "--filter"},
	} {
		b.repeated(f[0], f[1])
	}
	b.positional("text", false)
}

var searchTool = Tool{
	Name:        "search",
	Description: "Search the Vinted catalog. Mirrors `vinted search`; returns {items:[...],page,total_pages,total}.",
	InputSchema: object(with(searchFilterProps(), props{
		"page":  integer("first result page"),
		"pages": integer("how many result pages to fetch (max 5)"),
		"limit": integer("stop after this many items"),
	})),
	build: func(b *argv) {
		b.command("search")
		buildSearchFilters(b)
		b.integer("page", "--page")
		b.integer("pages", "--pages")
		b.integer("limit", "--limit")
	},
}

var searchFiltersTool = Tool{
	Name:        "search_filters",
	Description: "List the catalog filters for a search with every option id (sizes, conditions, colours, materials...), as the site's filter bar shows them. Pass catalog: options depend on the category. Mirrors `vinted search filters`.",
	InputSchema: object(with(searchFilterProps(), props{
		"code": list(text("load options only for these filter codes (size, status, color...)")),
	})),
	build: func(b *argv) {
		b.command("search", "filters")
		buildSearchFilters(b)
		b.repeated("code", "--code")
	},
}

// with returns base plus extra.
func with(base, extra props) props {
	for k, v := range extra {
		base[k] = v
	}
	return base
}

var itemShowTool = Tool{
	Name:        "item_show",
	Description: "Show one item in full, including reserved and sold flags. Mirrors `vinted item show`.",
	InputSchema: object(props{"item": text("numeric item id or a Vinted /items/... URL")}, "item"),
	build: func(b *argv) {
		b.command("item", "show")
		b.positional("item", true)
	},
}

var userShowTool = Tool{
	Name:        "user_show",
	Description: "Show a member's public profile and stats. Mirrors `vinted user show`.",
	InputSchema: object(props{"user": text("numeric user id or a /member/... profile URL")}, "user"),
	build: func(b *argv) {
		b.command("user", "show")
		b.positional("user", true)
	},
}

var userItemsTool = Tool{
	Name:        "user_items",
	Description: "List the items in a member's wardrobe. Mirrors `vinted user items`.",
	InputSchema: object(props{
		"user":  text("numeric user id or a /member/... profile URL"),
		"page":  integer("result page"),
		"limit": integer("items per page"),
	}, "user"),
	build: func(b *argv) {
		b.command("user", "items")
		b.integer("page", "--page")
		b.integer("limit", "--limit")
		b.positional("user", true)
	},
}

var userReviewsTool = Tool{
	Name:        "user_reviews",
	Description: "List the reviews a member received. Mirrors `vinted user reviews`.",
	InputSchema: object(props{
		"user": text("numeric user id or a /member/... profile URL"),
		"page": integer("result page"),
	}, "user"),
	build: func(b *argv) {
		b.command("user", "reviews")
		b.integer("page", "--page")
		b.positional("user", true)
	},
}

var categoryListTool = Tool{
	Name:        "category_list",
	Description: "Return Vinted's category tree with ids for search's catalog argument. Mirrors `vinted category list`.",
	InputSchema: object(props{}),
	build:       func(b *argv) { b.command("category", "list") },
}

var brandSearchTool = Tool{
	Name:        "brand_search",
	Description: "Find brand ids by name for search's brand argument. Mirrors `vinted brand search`.",
	InputSchema: object(props{"text": text("brand name or part of it")}, "text"),
	build: func(b *argv) {
		b.command("brand", "search")
		b.positional("text", true)
	},
}

var meShowTool = Tool{
	Name:        "me_show",
	Description: "Show the logged-in account's profile. Mirrors `vinted me show`.",
	InputSchema: object(props{}),
	build:       func(b *argv) { b.command("me", "show") },
}

var meFavoritesTool = Tool{
	Name:        "me_favorites",
	Description: "List the items the account favorited. Mirrors `vinted me favorites`.",
	InputSchema: object(props{
		"page":  integer("result page"),
		"limit": integer("items per page"),
	}),
	build: func(b *argv) {
		b.command("me", "favorites")
		b.integer("page", "--page")
		b.integer("limit", "--limit")
	},
}

var alertListTool = Tool{
	Name:        "alert_list",
	Description: "List the account's saved searches with the query each runs. Mirrors `vinted alert list`.",
	InputSchema: object(props{
		"all": flag("include recent (unsaved) searches"),
	}),
	build: func(b *argv) {
		b.command("alert", "list")
		b.boolean("all", "--all")
	},
}

var chatListTool = Tool{
	Name:        "chat_list",
	Description: "List the account's conversations, most recent first. Mirrors `vinted chat list`.",
	InputSchema: object(props{
		"unread": flag("only conversations with unread messages"),
		"limit":  integer("how many conversations"),
	}),
	build: func(b *argv) {
		b.command("chat", "list")
		b.boolean("unread", "--unread")
		b.integer("limit", "--limit")
	},
}

var chatShowTool = Tool{
	Name:        "chat_show",
	Description: "Show a conversation's messages, oldest first. Mirrors `vinted chat show`.",
	InputSchema: object(props{
		"conversation": text("numeric conversation id, from chat_list"),
		"limit":        integer("how many of the newest messages; 0 for all"),
	}, "conversation"),
	build: func(b *argv) {
		b.command("chat", "show")
		b.integer("limit", "--limit")
		b.positional("conversation", true)
	},
}

var watchCheckTool = Tool{
	Name:        "watch_check",
	Description: "Run the saved watches once and return the events they produced. Mirrors `vinted watch check`; empty list means nothing changed.",
	InputSchema: object(props{
		"names": list(text("watch name")),
		"all":   flag("run every watch of the profile, due or not"),
	}),
	build: func(b *argv) {
		b.command("watch", "check")
		b.boolean("all", "--all")
		b.positionals("names")
	},
}

// Schema

type schema struct {
	Type                 string   `json:"type"`
	Properties           props    `json:"properties"`
	Required             []string `json:"required,omitempty"`
	AdditionalProperties bool     `json:"additionalProperties"`
}

type props map[string]property

type property struct {
	Type        string    `json:"type"`
	Description string    `json:"description"`
	Enum        []string  `json:"enum,omitempty"`
	Items       *property `json:"items,omitempty"`
}

func object(p props, required ...string) schema {
	return schema{Type: "object", Properties: p, Required: required}
}

func text(desc string) property    { return property{Type: "string", Description: desc} }
func number(desc string) property  { return property{Type: "number", Description: desc} }
func integer(desc string) property { return property{Type: "integer", Description: desc} }
func flag(desc string) property    { return property{Type: "boolean", Description: desc} }

func oneOf(desc string, values ...string) property {
	return property{Type: "string", Description: desc, Enum: values}
}

func list(of property) property {
	return property{Type: "array", Description: of.Description, Items: &of}
}

// validate rejects unknown and missing keys before anything reaches the CLI,
// so a mistyped argument names itself instead of being silently dropped.
func (s schema) validate(in map[string]any) error {
	var unknown []string
	for k, v := range in {
		if _, ok := s.Properties[k]; !ok {
			unknown = append(unknown, k)
			continue
		}
		// An explicit null is a mistake, not an omission: passing it for a
		// required argument would otherwise reach the CLI as a missing one.
		if v == nil {
			return fmt.Errorf("argument %q must not be null; leave it out instead", k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		known := make([]string, 0, len(s.Properties))
		for k := range s.Properties {
			known = append(known, k)
		}
		sort.Strings(known)
		return fmt.Errorf("unknown argument %s. Accepted: %s", strings.Join(quoteAll(unknown), ", "), strings.Join(known, ", "))
	}
	for _, r := range s.Required {
		if _, ok := in[r]; !ok {
			return fmt.Errorf("argument %q is required", r)
		}
	}
	return nil
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strconv.Quote(s)
	}
	return out
}

// argv builders

type argv struct {
	in   map[string]any
	out  []string
	rest []string
	err  error
}

func (b *argv) command(parts ...string) { b.out = append(b.out, parts...) }

func (b *argv) fail(key string, want string, got any) {
	if b.err == nil {
		b.err = fmt.Errorf("argument %q must be %s, got %T", key, want, got)
	}
}

// value returns the raw input for key, and whether it was given at all.
func (b *argv) value(key string) (any, bool) {
	v, ok := b.in[key]
	return v, ok && v != nil
}

func (b *argv) text(key, flagName string) {
	v, ok := b.value(key)
	if !ok {
		return
	}
	s, ok := v.(string)
	if !ok {
		b.fail(key, "a string", v)
		return
	}
	b.out = append(b.out, flagName, s)
}

func (b *argv) boolean(key, flagName string) {
	v, ok := b.value(key)
	if !ok {
		return
	}
	on, ok := v.(bool)
	if !ok {
		b.fail(key, "a boolean", v)
		return
	}
	if on {
		b.out = append(b.out, flagName)
	}
}

func (b *argv) number(key, flagName string) {
	v, ok := b.value(key)
	if !ok {
		return
	}
	f, ok := v.(float64)
	if !ok {
		b.fail(key, "a number", v)
		return
	}
	b.out = append(b.out, flagName, strconv.FormatFloat(f, 'f', -1, 64))
}

func (b *argv) integer(key, flagName string) {
	v, ok := b.value(key)
	if !ok {
		return
	}
	f, ok := v.(float64)
	if !ok {
		b.fail(key, "an integer", v)
		return
	}
	if f != float64(int64(f)) {
		b.err = fmt.Errorf("argument %q must be a whole number, got %v", key, f)
		return
	}
	b.out = append(b.out, flagName, strconv.FormatInt(int64(f), 10))
}

// repeated emits one flag occurrence per array element, the way the CLI takes
// repeatable flags such as --condition.
func (b *argv) repeated(key, flagName string) {
	for _, s := range b.strings(key) {
		b.out = append(b.out, flagName, s)
	}
}

func (b *argv) positional(key string, required bool) {
	v, ok := b.value(key)
	if !ok {
		return
	}
	s, ok := v.(string)
	if !ok {
		b.fail(key, "a string", v)
		return
	}
	if s == "" {
		if required {
			b.err = fmt.Errorf("argument %q must not be empty", key)
		}
		return
	}
	b.rest = append(b.rest, s)
}

func (b *argv) positionals(key string) { b.rest = append(b.rest, b.strings(key)...) }

func (b *argv) strings(key string) []string {
	v, ok := b.value(key)
	if !ok {
		return nil
	}
	raw, ok := v.([]any)
	if !ok {
		b.fail(key, "an array of strings", v)
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		s, ok := e.(string)
		if !ok {
			b.fail(key, "an array of strings", e)
			return nil
		}
		out = append(out, s)
	}
	return out
}
