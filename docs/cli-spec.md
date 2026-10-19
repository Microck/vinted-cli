# vinted-cli spec

Status: draft. Vocabulary in `../AGENTS.md`.
Endpoint facts come from live probes on 2026-10-17 against `www.vinted.com` with a real account; the API is unofficial and may drift.

## 1. Name

Binary `vinted`. Module `github.com/Microck/vinted-cli`. Single static Go binary.

## 2. One-liner

Search, track and chat on Vinted from the terminal, with your own account.

## 3. Usage

```
vinted [global flags] <noun> <verb> [args] [flags]
```

Noun-verb throughout. No catch-all subcommand, no abbreviations.

## 4. Command tree

```
auth       login | status | refresh | logout | service (enable|disable|status|run|stop)
profile    list | use | remove
search     [text...] | filters
category   list
brand      search
alert      list
item       show | images | open | favorite | unfavorite | create | edit | delete | hide | unhide
user       show | items | reviews
me         show | items | favorites
chat       list | show | send | start | open | offer (accept|decline) | reserve
watch      add (search|item|seller) | list | remove | check | run | events | service (install|uninstall|status)
sink       list | test
mcp        (no verb: serves) | install claude-code|codex|cursor
config     path | get | set | list
doctor
cooldown   [clear]
skills     list | get
update
completion <shell>
```

## 5. Endpoints

Every host, path and raw shape lives in `internal/vinted`. `{base}` is the Profile's Vinted
domain (`https://www.vinted.com` by default, `VINTED_BASE_URL` overrides it for tests).

| command | request | notes |
|---|---|---|
| `auth refresh`, the session engine before expiry | `POST {base}/web/api/auth/refresh` body `{}` | sent by the Galleton engine, never the CLI, with the cookies and the web app's headers; 200 `{access_token, refresh_token, expires_in, ...}` and both token cookies rotated in `Set-Cookie`, which the engine keeps. A refused refresh is exit 3 |
| `auth login`, `me show` | `GET /api/v2/users/current` | `{"user":{...}}` |
| `user show` | `GET /api/v2/users/{id}` | public profile |
| `user items`, `me items` | `GET /api/v2/wardrobe/{user_id}/items?page=&per_page=` | `{"items":[...],"pagination":{current_page,total_pages,total_entries,per_page}}` |
| `user reviews` | `GET /api/v2/user_feedbacks?user_id=` | |
| `me favorites` | `GET /api/v2/users/{id}/items/favourites?per_page=` | |
| `item favorite`, `item unfavorite` | `POST /api/v2/user_favourites/toggle` | needs `x-csrf-token`, read from page HTML. A toggle, so the CLI reads the current state first |
| `brand search` | `GET /api/v2/brands?keyword=` | |
| `category list` | `GET /api/v2/item_upload/catalogs` | full Catalog tree |
| `doctor`, `chat list --unread` | `GET /api/v2/conversations/stats` | `{"unread_msg_count"}` |
| `chat list` | `GET /api/v2/inbox?page=&per_page=` | unverified accounts get 400 code 115 `user_verification_required`, reported as an auth error with a hint |
| `chat show` | `GET /api/v2/conversations/{id}` | |
| `chat send`, `chat open` | `POST /api/v2/conversations/{id}/replies` | `chat open` polls `GET /api/v2/conversations/{id}` every `--interval` |
| `chat offer` (buyer) | `POST /api/v2/transactions/{tid}/offer_requests` body `{"offer_request":{"price":"20.00","currency":"EUR"}}` | `tid` is the conversation's `transaction.id` |
| `chat offer` (seller) | `POST /api/v2/transactions/{tid}/offers` body `{"offer":{"price","currency"}}` | seller when `transaction.user_side == "seller"` |
| `chat offer accept\|decline` | `PUT /api/v2/transactions/{tid}/offer_requests/{id}/accept\|reject` | the latest `offer_request_message` from the buyer with status 10 (pending) |
| `chat reserve` | `PUT /api/v2/transactions/{tid}/reservation` body `{"reserved":bool}` | |
| `item create` | `POST /api/v2/photos` (multipart `photo[type]=item`, `photo[temp_uuid]`, `photo[file]`) per image, then `POST /api/v2/item_upload/items` (header `X-Upload-Form: true`) or `POST /api/v2/item_upload/drafts` with `--draft` | body `{"item"\|"draft":{...form...},"push_up":false,"parcel":null,"upload_session_id"}`; form fields as the web upload form: `title, description, price, currency, catalog_id, brand_id, brand, color_ids, package_size_id, assigned_photos:[{id,orientation}], item_attributes:[{code,ids}]` |
| `item create`, `item edit` (validation) | `POST /api/v2/item_upload/attributes` body `{"attributes":[{"code":"category","value":[id]}]}`, `GET /api/v2/item_upload/brands?keyword=&category_id=`, `GET /api/v2/item_upload/colors`, `GET {gateway}/shipping-estimation/external/catalogs/{id}/package_sizes` | size, condition and other category attributes with their options |
| `item edit` | `GET /api/v2/item_upload/items/{id}`, then `PUT` the same path with the edited form | untouched fields are sent back as stored |
| `item delete` | `POST /api/v2/items/{id}/delete`; with `--draft` `DELETE /api/v2/item_upload/drafts/{id}` | |
| `item hide`, `item unhide` | `PUT /api/v2/items/{id}/is_hidden` body `{"is_hidden":bool}` | |
| `alert list`, `watch add search --from-alert` | `GET {gateway}/search-bar/v2/users/{id}/previous_searches[/{search_id}]` | needs a `Locale` header. `type_saved` entries are saved searches, `type_recent` the search box history (`--all`) |
| `search` | `GET /catalog?search_text=&page=&order=&price_from=&price_to=&currency=&catalog[]=&brand_ids[]=&{code}_ids[]=...` | **server-rendered page**, not JSON |
| `search filters` | `GET {gateway}/svc-filters/filters?search_text=&attribute_ids[catalog]=&attribute_ids[{code}]=...`, then `GET {gateway}/svc-filters/filters/facets?...&filter_code={code}` per filter | the list names the filters; options of filters marked `is_lazy` come from `facets`, as the web app's filter bar loads them |
| `item show`, `item images` | `GET /items/{id}` | **server-rendered page**, not JSON |
| `item images --download` | `GET https://images*.vinted.net/t/...` per photo | signed CDN links from the item page; paced and counted like any request |

`{gateway}` is Vinted's API gateway, resolved as the web app does: `https://api.www.vinted.com`
for vinted.com, `https://api.{domain}` for the other sites, and `{base}/web/gateway` for a
non-Vinted base URL (tests). Every write above answers 400 code 115
(`user_verification_required`) until Vinted has verified the account: exit 3 with the
verification page as the suggested fix. A 422 `validation_error` names the offending fields
and is exit 2.

Vinted has no JSON search or item API on the public web: `/api/v2/catalog/items` and
`/api/v2/items/{id}` answer an HTML 404. The catalog and item pages are Next.js
server-rendered; their data sits in the escaped React Server Components payload
(`\"items\":[{\"id\":...,\"productItem\":{\"title\",\"url\",\"price\":{\"amount\",\"currencyCode\"},\"totalItemPrice\",\"favouriteCount\",\"thumbnailUrls\"...}}]`
and `\"total_entries\":960`). The parser extracts it there. A page whose payload no longer
matches is exit 7, never an empty result.

Only the core of a response is strict: the ids and the object a command exists to return.
An optional part in a shape the CLI does not know (a badge given as a string, an item block
it has no reader for, one odd entry in a list) does not fail the command. Its raw JSON goes
into the output's `unparsed` object, keyed by its JSON path (`plugins.user_info_header.badges.1`,
`items.3`), so no data is dropped.

Datadome and Cloudflare front every host. Requests carry a real browser user agent;
a challenge page is exit 5.

### auth

- `auth login [--profile NAME] [--cookies FILE | --cookies-stdin] [--domain HOST] [--service | --no-service]`
  Default is the cookie import prompt: it asks for a cookie export, parsed by Galleton's
  `cookiefile` package (Netscape `cookies.txt`; Cookie-Editor, EditThisCookie, DevTools or
  Playwright JSON, bare or wrapped as `{"url":..., "cookies":[...]}`; or a raw `a=b; c=d`
  header). The export is narrowed to the Vinted sites, then to the site's origins; the
  site's other cookies are kept with the tokens, because Datadome cookies matter as much.
  `refresh_token_web` is required. The cookies are imported into the session engine as a
  new session beside the Profile's current one; login then validates with
  `GET /api/v2/users/current` through the engine and only then stores the Session reference
  and the domain (from `--domain`, else the domain of the token cookies, as
  `profiles.NAME.domain` in config) and deletes the session it replaced. A rejected import
  is removed from the engine again and the Profile keeps its previous session.
  Implicitly creates the Profile; the first Profile becomes default. The Session records
  `refresh_expires`, the expiry of the imported `refresh_token_web` (cookie expiry, else its
  JWT `exp`), and `refresh_issued`. `refresh_issued` is the token's JWT `iat` (else the import time), and when the login itself,
  `auth status --check`, `auth refresh` or `doctor` see a later engine renewal
  (`last_refresh`), the expiry moves forward by the same lifetime, since each renewal issues
  a new refresh token. After a successful login, when stdin and stderr are terminals, `--no-input`
  is not set, `CI` is not `true` and no `VINTED_GALLETON_*` override is set, and the renewal
  service is not installed yet, it asks on stderr `keep this login alive in the background?
  [Y/n]`. Yes or an empty line runs the `auth service enable` code path and reports the unit on
  stderr; end of input counts as no; no prints ``run `vinted auth service enable` later to turn it on``. `--service`
  answers yes without asking and always runs the enable again, which retries a half-failed
  one (works with `--no-input`; a failure is exit 1 with the login kept), `--no-service`
  answers no; the two are exclusive (exit 2). The status is printed on stdout after the
  choice.
- `auth status [--check]` shows the stored Profile, account, `source: "galleton"`,
  `session_id`, and a `cooldown` object while the site is paused after a block. It is
  offline. `--check` adds the engine's view (`engine`: status, last and next renewal, access
  token expiry, cookie count) and an authenticated call, reported as `session_valid` (exit 5
  during a cooldown). A Profile saved by an older version reports `needs_login`. It also
  shows `refresh_expires` and, when that is within 48 h and the renewal service is not
  installed, a `warning` (also printed on stderr; never with `VINTED_GALLETON_*` overrides,
  whose owner runs the engine); the exit code is unchanged.
- `auth refresh` asks the engine to renew now (through the request gate) and prints status
  with the engine's view. A refused renewal is exit 3 and needs a new browser export.
- `auth logout [--yes]` drops the stored reference, then deletes the engine's session. A
  deletion the engine refuses stays queued in `credentials.toml` and is retried by the next
  command that reaches the engine. This does not revoke the browser login.
- `auth service enable|disable|status|run|stop` manages the session engine's optional
  unattended renewal: `enable` installs a systemd user unit, LaunchAgent or Windows logon
  task running `auth service run`, with the same frozen environment as watch units (and the
  same refusal of a `VINTED_PROXY` with credentials); `status` never starts the engine;
  `stop` refuses while clients hold leases.

Session mechanics: the account's cookies live only in the pinned Galleton daemon bundled in
the binary (ADR 0002, `docs/galleton.md`). It renews the access token five minutes before the
`expires_in` Vinted returns, keeps every cookie a response sets except that only a renewal may
set the two tokens, and relays every logged-in request with the cookies attached. A 401
asks it to renew once and retries. Requests pass the CLI's gate first; a block also pauses the
engine's provider for the cooldown. The account id is recorded at login. No password ever
touches the CLI, and after the import no cookie enters its process.

### profile

- `profile list` (name, account, default marker, engine session ID, login time, `needs_login`)
- `profile use NAME` sets the default
- `profile remove NAME [--yes]` removes the Session and its Watches

Selection order: `--profile` > `VINTED_PROFILE` > default in config.

### search

- `search [text...] [--price-from N] [--price-to N] [--currency CODE]
  [--order relevance|newest_first|price_low_to_high|price_high_to_low]
  [--catalog ID]... [--brand ID]... [--size ID]...
  [--condition new_with_tags|new_without_tags|new|very_good|good|satisfactory|not_fully_functional|ID]...
  [--color ID]... [--material ID]... [--pattern ID]... [--video-game-rating ID]...
  [--platform ID]... [--model ID]... [--storage ID]... [--sim-lock ID]... [--language ID]...
  [--attr CODE=ID[,ID]]... [--filter key=value]... [--page N] [--pages N] [--limit N]`
  Reads the catalog page (`/catalog`). Id flags repeat. Every filter is a filter code of
  Vinted's filter service sent as `{code}_ids[]`: `--condition` is `status`, `--pattern`
  `patterns`, `--video-game-rating` `video_game_rating`, `--platform` `video_game_platform`,
  `--model` `brand_collection`, `--storage` `internal_memory_capacity`, `--sim-lock`
  `sim_lock`, `--language` `language_book` (all seen live on vinted.com in Oct 2026, with
  the catalog page echoing them back as selected). `--attr` sets any other code `search
  filters` lists; a code with its own flag is exit 2. `--filter` passes any other catalog
  query parameter through verbatim; it repeats, repeated keys accumulate and commas are kept.
  JSON output is `{items, page, total_pages, total}`; `--format jsonl` prints that document on
  one line, as every non-list result (use `jq -c '.items[]'` for one item per line).
  `--pages` is at most 5 (exit 2 above). `total` tops out at 960 (10 pages of 96), as seen live.
- `search filters [text...] [search flags] [--code CODE]...` lists the filters the catalog
  offers for that search, as the filter bar shows them: `id`, `code`, `title`, `type`
  (`list`, `list_search`, `grid`, `hybrid_price`), `multi`, `params` (the catalog query keys)
  and `options` (`id`, `title`, `description`, `count`, `hex`/`image` swatch, nested
  `options`; headings carry a `type` such as `group` and no id). Options depend on the
  category and on what is already selected. One request for the list and one per filter for
  its options; `--code` loads only those (an unknown code is exit 2). `list_search` filters
  (brand) list only popular options; `brand search` finds the rest.

### category / brand

- `category list` prints the Catalog tree from `GET /api/v2/item_upload/catalogs`.
- `brand search KEYWORD` prints Brands (`id`, `title`, `slug`, item count) for `--brand`.

### alert

- `alert list [--all]` lists the account's saved searches (`id`, `title`, `subtitle`, `saved`,
  `new_items`, `last_visited_at`, `query`), read-only. `query` is the search it runs, in the
  `search` parameter shape. `--all` adds the recent searches the search box remembers.

### item

Item argument accepts a numeric id or any Vinted item URL (`/items/{id}-slug`, any domain).

- `item show ITEM [--seller]` reads the item page: title, description, price, total price
  and `buyer_protection_fee`, `shipping` (`price`, `currency`, `free`, `from_price` when
  several options exist and the price is the cheapest, `delivery`), brand and `brand_id`,
  size, condition, `color`, `attributes` (every detail line as shown: `code`, `title`,
  `value`, e.g. material or measurements when the listing has them), `category_id` and
  `category_path`, `uploaded` (the page's relative wording; there is no exact date),
  favourites, flags (reserved, sold, hidden, favorited, `can_buy`), `images` and `photos`
  (`url`, `original_width`, `original_height`, `main`, `dominant_color`), and `seller`
  (`login`, `rating` 0-5 as `round(5*feedback_reputation, 2)`, `reviews`, `business`,
  `last_seen` wording, `badges`, `info`). Views are not on the page. `--seller` adds
  `seller.profile`, the public profile from `GET /api/v2/users/{id}` (city, country, last
  login): one more request.
- `item images ITEM [--download DIR [--limit N]]` returns the image URLs in gallery order. JSON
  is an array of URL strings; JSONL emits one string per line; pretty emits one unadorned URL
  per line. The URLs are the f800 rendition, which can be smaller than the original (a
  1200x1600 upload is served at 600x800). `--download` saves them as `DIR/ITEMID-N.EXT` and
  prints `[{url, file, bytes, content_type}]`; every photo is one paced, counted request, and
  only `*.vinted.net` hosts are fetched. A plain 403 from the image host (bad signature) is an
  error, not a block.
- `item open ITEM` opens the web page in the browser without any request: an http(s) item URL
  on a Vinted site (`vinted.<tld>` or `www.vinted.<tld>` for a TLD Vinted runs) as given, a
  bare id as `{base}/items/{id}`, which Vinted redirects to the slugged URL. Other schemes and
  hosts, look-alikes such as `vinted.example.com` included, are exit 2.
- `item favorite ITEM` / `item unfavorite ITEM` set the Favorite through the toggle endpoint;
  already being in the requested state is a no-op. When the item page does not state the
  current favourite state, nothing is toggled and the command exits 7.

Own listings (the CLI checks the item's seller is the account before any write):

- `item create --title T --description D --price N --category ID|NAME --condition NAME|ID
  --image FILE... [--size NAME|ID] [--brand NAME|ID] [--color NAME|ID]... [--package-size
  small|medium|large|ID] [--attr code=NAME|ID]... [--currency CODE] [--draft]` (alias
  `publish`). Category must be a leaf. Attribute values, brand, colours (at most 2) and
  package size are resolved against what Vinted offers for the category before any upload;
  missing required attributes are exit 2, as is an `--image` file that is not an image or is
  larger than 32 MiB. Publishing (not `--draft`) also needs `--brand`
  and `--color` for most categories; Vinted rejects a listing without them as exit 2 naming
  the fields. Package size defaults to the smallest offered; currency to the account's.
  `--draft` saves a draft only the account sees. Prints the Listing (`id`, `title`, `price`,
  `currency`, `url`, `draft`, ...); fields the answer leaves out are the submitted values.
- `item edit ITEM [same field flags]` changes only the flags given; `--image` replaces every
  photo. Untouched fields, colours included, are sent back as stored. No change flags is
  exit 2.
- `item delete ITEM [--yes] [--draft]` deletes a listing (or, with `--draft`, the draft with
  that id). Prompts unless `--yes`; non-interactive without `--yes` is exit 2.
- `item hide ITEM`, `item unhide ITEM` toggle catalog and profile visibility.

Vinted has no item-level reserve or mark-as-sold call: reserving is per buyer (`chat
reserve`), and marking sold happens through the order flow of a conversation.

### user / me

- `user show USER`, `user items USER [--page N] [--limit N]`, `user reviews USER`
  (public; USER is a numeric id or a `/member/{id}-login` URL).
- `me show`, `me items [--page N] [--limit N]`, `me favorites [--page N] [--limit N]`
  (authenticated).

### chat

Conversation argument: the numeric Conversation id.

- `chat list [--unread] [--limit N]` from the inbox.
- `chat show CONV [--limit N]` Messages oldest first. Offer events read
  `offer request: $60.00 (pending)` / `counter offer: ...`.
- `chat send CONV TEXT` (`-` reads TEXT from stdin).
- `chat start ITEM TEXT` opens a Conversation with the Item's seller and sends the first
  Message. Vinted answers the create call with the existing Conversation about that Item,
  so a second start lands in the same thread. If Vinted opens the Conversation but refuses
  the Message, the error names the Conversation id.
- `chat open CONV [--interval DUR]` line-mode chat: prints recent history, then polls every
  `--interval` (default 30s, minimum 10s) for new Messages while each typed line is sent.
  `/quit` or EOF exits. With `--format jsonl` incoming Messages print as JSON objects. A
  block ends it with exit 5, including a scheduled engine renewal blocked between polls,
  which each poll checks for first.
- `chat offer CONV AMOUNT [--currency CODE]` sends a price offer (buyer: offer request;
  seller: counter offer). AMOUNT is a positive decimal with at most two fractional digits.
  `chat offer accept CONV` / `chat offer decline CONV` answer the latest pending offer
  request (seller only). Prints the Offer (`id`, `conversation`, `transaction`, `amount`,
  `currency`, `status`, `kind`).
- `chat reserve CONV [--off]` reserves the conversation's item for that buyer (seller only).

### watch

Watches, Checks, Events and Sinks are defined in `AGENTS.md`.

- `watch add search [text...] [search flags] --name NAME [--notify SINK]... [--interval DUR] [--pages N] [--emit-initial]`
  or `watch add search --from-alert ID --name NAME [...]`, which copies a saved search's
  query (exclusive with text and search flags). Without `--order` the watch reads
  `newest_first`. Vinted reshuffles result pages between loads (older and bumped listings
  drift in and out, even under `newest_first`), so a search Check emits `item.new` only for
  unseen items whose id is above the newest id of the watch's first Check (`since_id`;
  Vinted ids grow with listing time). Older unseen items are recorded silently, every id
  ever seen is kept, and items leaving the pages are never reported.
- `watch add item ITEM --name NAME [--notify]... [--interval DUR] [--emit-initial]`
- `watch add seller USER --name NAME [--notify]... [--interval DUR] [--emit-initial]`
  `add` runs the first Check immediately as a silent baseline; with `--emit-initial` that
  baseline is reported (and delivered to Sinks) as `item.new` / `seller.new_item` Events.
- `watch list`, `watch remove NAME [--yes]`.
- `watch check [NAME...] [--all] [--jitter DUR]` runs one Check per named Watch (default: all
  due), one after another. Prints Events, delivers them to the Watch's Sinks, updates state.
  Idempotent; exit 0 even when nothing changed. `--jitter` first sleeps a random time up to
  DUR (the service units pass a tenth of their interval). A block or a rejected Session
  stops the run.
- `watch run [--interval DUR]` foreground loop: due Checks every interval until Ctrl-C.
  Interval default 15m, floor 15m (exit 2 below), plus up to 10% jitter. Each tick has its
  own request budget. A block ends the loop with exit 5.
- `watch events [NAME] [--since DUR] [--limit N]` reads stored Event history.
- `watch service install [--interval DUR]` (default 15m, floor 15m) writes a systemd user
  service+timer (Linux) or a launchd agent (macOS) that runs
  `vinted watch check --all --jitter <interval/10> --profile ...`. On Windows it
  prints the `schtasks` command to run by hand. The unit pins `HOME`, the XDG variables,
  `VINTED_CONFIG`, `VINTED_BASE_URL`, `VINTED_PROXY`, `HTTP(S)_PROXY`/`NO_PROXY`,
  `SSL_CERT_FILE`/`SSL_CERT_DIR` and `VINTED_GALLETON_*` as they stood at install time, because a scheduler starts jobs with a bare environment. No
  credential is ever written: unit files are world-readable, so install refuses a proxy
  variable with credentials.
  On Linux install also runs `loginctl enable-linger`. `status` says
  `not installed`, or what the scheduler reports. `uninstall` removes the units and the log.
  Output is appended to `$XDG_STATE_HOME/vinted-cli/vinted-watch-<profile>.log`, rotated to
  `.log.1` past 1 MiB.

Event types: `item.new`, `item.price_changed`, `item.reserved`, `item.unreserved`,
`item.sold`, `item.removed`, `seller.new_item`. Event shape:

```json
{"type":"item.price_changed","watch":"levis","profile":"default","at":"2026-10-17T10:40:00Z",
 "item":{"id":4923018331,"title":"...","price":25,"currency":"EUR","url":"..."},
 "change":{"from":30,"to":25}}
```

Search Watches diff the first `--pages` pages (default 1, max 5): new ids and price changes
on already-seen ids. Item Watches diff the item page. Seller Watches diff the Member's
wardrobe, at most 10 pages of 96 per Check; past that, removals are not reported because the
tail was not read. `watch add` refuses `--interval` under 15m (default 15m) and `--pages`
over 5 before sending anything.

### sink

Sinks live in config, not in the database:

```toml
[sinks.phone]
type = "ntfy"
url = "https://ntfy.sh"
topic = "vinted"
token_file = "~/.config/vinted-cli/ntfy-token"   # optional

[sinks.discord]
type = "webhook"
url = "https://discord.com/api/webhooks/..."
# body: the Event JSON; set template = "discord" for a {content: ...} wrapper

[sinks.script]
type = "exec"
command = ["~/bin/on-vinted-event"]
# receives one Event JSON on stdin per Event
```

- `sink list`, `sink test NAME` (sends a synthetic Event).

Configure an exec sink entirely through the CLI with a TOML array of strings:

```sh
vinted config set sinks.script.type exec
vinted config set sinks.script.command '["/home/me/bin/on-vinted-event", "--verbose"]'
vinted sink test script
```

### mcp

`vinted mcp` serves an agent harness over the Model Context Protocol, stdio transport:
newline-delimited JSON-RPC 2.0, methods `initialize`, `ping`, `tools/list`, `tools/call`.
No MCP SDK.

- Each tool is defined as a CLI argv and runs the command in-process, so a tool's output is
  the same JSON the command prints. A failing command returns a tool error carrying the
  `--error-format json` envelope; a malformed call is JSON-RPC `-32602`.
- Tools: `search`, `search_filters`, `item_show`, `user_show`, `user_items`, `user_reviews`,
  `category_list`, `brand_search`, `me_show`, `me_favorites`, `alert_list`, `chat_list`,
  `chat_show`, `watch_check`. Arguments mirror the flags with dashes as underscores
  (`price_to`, `sim_lock`); repeatable flags (`catalog`, `attr`, `filter`, ...) take arrays of
  strings. A test fails when a search flag has no tool argument. Unknown argument names are
  refused.
- Not exposed: `chat send`, `chat start`, `chat open`, `chat offer`, `chat reserve`,
  `item favorite`/`unfavorite` and every listing change (`item create`, `edit`, `delete`,
  `hide`, `unhide`). Writing to other people or changing the account stays with the person
  at the terminal.
- Every invocation re-reads config and credentials, so a long-running server follows a new
  login, and takes and releases its own session engine lease.
- `vinted mcp install claude-code|codex|cursor` writes the entry into the harness config.
  Other entries and keys the CLI does not own survive. Output is
  `{harness,path,action,command,args}` with action `added`, `updated` or `unchanged`. The
  entry pins `--profile` only when one was chosen with `--profile` or `VINTED_PROFILE`; a bare
  install follows the config's default profile, including a later `vinted profile use`.

### config / doctor / skills / update / completion

- `config path | list | get KEY | set KEY VALUE` on `config.toml`.
- `config set` validates `request_interval` (floor 1s), `watch.interval` (floor 15m),
  `proxy` (scheme, host:port, no credentials since config.toml is world-readable) and a
  Sink's `type` (`ntfy`, `webhook`, `exec`) and `template` (`discord`, `slack` or unset). A
  rejected value is exit 2 and leaves the file untouched.
- `doctor` checks: config parses, Sinks, proxy (shown without credentials), cooldown, Vinted
  reachable, Session works, service unit state, and a `renewal` line marked as a warning
  (`"warning": true`, still `ok`) when the refresh token expires within 48 h and the renewal
  service is not installed. Exit 1 if any check fails; exit 5, without any network check,
  while a cooldown lasts.
- `cooldown` shows the cooldown of the Profile's site (`{site, blocked, blocked_at,
  blocked_until, remaining, cause, request_interval, file}`); `cooldown clear [--yes]` lifts
  it (prompts, or needs `--yes`) and forgets the cached tokens. See section 11.
- `skills list | get NAME` prints the embedded agent-facing docs (`vinted-usage`).
- `update [--check] [--yes]` replaces a direct-binary install with the latest stable release
  after checksum verification; package-manager installs are pointed at their manager. While
  no stable release is published it exits 4 ("no stable release of vinted-cli is published
  yet"); an unreachable GitHub is exit 6. Development builds are refused with exit 2.
- `completion bash|zsh|fish|powershell` (cobra).

## 6. Global flags

| flag | default | meaning |
|---|---|---|
| `--profile NAME` | config default | which Profile acts |
| `--format json\|pretty\|jsonl\|toon` | `json` | stdout rendering |
| `--no-color` | off | also honours `NO_COLOR`, `TERM=dumb`, non-TTY |
| `--error-format text\|json` | `text` | stderr error rendering |
| `--no-input` | off | never prompt; missing input is an error |
| `--debug` | off | HTTP request/response log on stderr, secrets redacted |
| `-h, --help`, `--version` | | |

## 7. I/O contract

- stdout: data only, one JSON document per command (or JSON lines).
- stderr: errors, prompts, progress, debug.
- Prompts only when stdin, stdout and stderr are all TTYs and `--no-input` is absent.
- Secrets never appear in argv: cookies come from a file, stdin or the prompt, and the proxy
  reaches the session engine through its environment.

## 8. Exit codes

| code | meaning |
|---|---|
| 0 | success (including "nothing changed") |
| 1 | generic failure |
| 2 | usage: bad flags, bad id or URL, interval/page caps, a command past its 60-request budget, an invalid `proxy` or `request_interval` |
| 3 | auth: no Session, Session rejected, renewal refused or uncertain, a Profile saved by an older version, account verification required |
| 4 | not found: item/user/conversation/watch |
| 5 | blocked: Datadome/Cloudflare challenge or block page, 429, a 403 without a Vinted error body, or a request refused during the cooldown that follows. Never retried |
| 6 | network / timeout, including an unreachable proxy (never falls back to direct) |
| 7 | API changed: known endpoint or page returned an unexpected shape or status |
| 130 | interrupted |

## 9. Errors

Text errors are one sentence, lowercase-led, consequence plus remedy. JSON envelope on stderr
with `--error-format json`: `{code, category, retryable, message, http_status, endpoint,
suggested_commands, issue_url}`.

Exit 7 always prints an `issue_url` pointing at
`https://github.com/Microck/vinted-cli/issues/new?template=api-change.yml&...` with CLI
version, OS/arch, command path (no argument values), endpoint path, HTTP status and the first
300 characters of the response body with tokens redacted.

## 10. Config, state, env

| what | path | mode |
|---|---|---|
| config | `$XDG_CONFIG_HOME/vinted-cli/config.toml` | 0644 |
| credentials | `$XDG_CONFIG_HOME/vinted-cli/credentials.toml` | 0600, locked/atomic updates |
| session engine | `$XDG_CONFIG_HOME/vinted-cli/galleton/` (encrypted vault, extracted engine, API token) | 0700 dir, 0600 files |
| state | `$XDG_DATA_HOME/vinted-cli/state.db` (SQLite) | 0600 |
| logs, throttle state | `$XDG_STATE_HOME/vinted-cli/` (`throttle.json` 0600, locked/atomic updates) | |

`credentials.toml` holds one table per Profile: the session engine's session ID
(`galleton_id`), the account id and login, and when it was logged in. No cookie. It is never
relocatable by config, only by XDG variables.

```toml
default_profile = "default"

[profiles.default]
domain = "www.vinted.fr"

[watch]
interval = "15m"
```

Env: `VINTED_PROFILE`, `VINTED_CONFIG` (config file path), `VINTED_ERROR_FORMAT`,
`VINTED_BASE_URL` (tests), `VINTED_NO_UPDATE_CHECK`, `NO_COLOR`, `VINTED_PROXY`,
`VINTED_GALLETON_DIR` / `VINTED_GALLETON_URL` / `VINTED_GALLETON_TOKEN_FILE` (use an
externally run Galleton daemon instead of the bundled one).
`HTTPS_PROXY`/`HTTP_PROXY` apply only when no proxy is configured, to the session engine
too; `VINTED_PROXY` (or
config `proxy`, e.g. `socks5h://127.0.0.1:1080`, `http://host:port`) sends every Vinted
request through that proxy, the session engine's renewals included, ignores `NO_PROXY`, and
fails with exit 6 rather than going direct when the proxy is unreachable. A running engine
keeps the proxy it started with; a command configured with another one refuses to attach. An invalid proxy fails every request with exit 2.
`request_interval` (top-level config key, default `2s`, floor `1s`) is the gap between two
requests to one site.
Precedence: flags > env > config.

## 11. Safety

Vinted's bot protection blocks sessions that look automated. The CLI enforces, with no flag
to turn it off:

- Pacing: at most one request per `request_interval` (2s) plus 0-1s jitter per site, across
  every process and Profile, coordinated through `$XDG_STATE_HOME/vinted-cli/throttle.json`
  under a file lock. Slots are reserved under the lock, so parallel processes queue.
- Cooldown: a block (section 8, code 5) is never retried and records `blocked_until` = now +
  6h, or the response's `Retry-After` if longer (capped at 7 days), for the site. Until then
  every request fails before touching the network with exit 5 and the time left. `cooldown`,
  `doctor` and `auth status` (`cooldown` field) show it; only `cooldown clear` ends it early.
  A block also drops the cached guest jar and CSRF tokens.
- Budget: one command (one `watch run` tick, one `chat open` poll, one MCP tool call) sends
  at most 60 requests, then fails with exit 2.
- Page caps: `search --pages` ≤ 5, search watches ≤ 5 pages, seller watches ≤ 10 pages.
- Schedules: watch intervals ≥ 15m; `chat open --interval` ≥ 10s (default 30s).
- Fewer requests: the CSRF token (per account, 1h) and the guest jar (until its token
  expires) are cached in `throttle.json`; a CSRF token rejected with 403 is replaced once.
- Consistent headers: one Chrome-on-Linux user agent with matching `Sec-CH-UA` client hints,
  `Sec-Fetch-*` for page loads and API calls, and `Accept-Language` from the site's country.

- `item delete` cannot be undone on Vinted's side, so it prompts or needs `--yes`, like
  `profile remove` and `watch remove` for local state. Listing changes refuse items the
  account did not list before writing anything.
- `watch check` and `watch run` are idempotent and crash-only: state is written after Events
  are delivered, so a crash re-emits rather than loses.
- Never phones home. No analytics.

## 12. Examples

```sh
vinted auth login --cookies ~/Downloads/vinted.txt --profile fr --domain vinted.fr
vinted search "levis 501" --price-to 30 --order newest_first --format pretty
vinted search "air max" --brand 53 --size 208 --condition very_good --limit 20
vinted item show https://www.vinted.com/items/4923018331-levis-501
vinted item favorite 4923018331
vinted item create --title "levi's 501" --description "worn twice" --price 25 --category 1840 --condition "very good" --size W32 --image front.jpg
vinted item delete 4923018331 --yes
vinted alert list --format pretty
vinted watch add search --from-alert 4650918777 --name saved
vinted chat offer 18273645 20
vinted watch add search "levis 501" --price-to 30 --name levis --notify phone
vinted watch check --all --format jsonl | jq -r 'select(.type=="item.new") | .item.url'
vinted chat start 4923018331 "hi, is this still available?"
vinted mcp install claude-code
```

## 13. Not in v1

Email+password login, checkout and payments, marking an item sold outside the order flow,
shipping (never), settings edits (never), realtime chat push (`chat open` polls).

## 14. Layout

```
cmd/vinted/main.go   entrypoint; lives under cmd/ so `go install` names the binary vinted
cmd/bundle-galleton/ builds the pinned session engine for embedding
internal/cli/        one file per noun
internal/vinted/     client: account and guest transport, engine adapter, page parsing, items, users, chat, errors
internal/galleton/   bundled session engine: pin, extraction, host process, SDK wrapper
internal/store/      sqlite schema, watches, events, seen-state
internal/watch/      check logic per target type, diffing, event construction
internal/sink/       ntfy, webhook, exec
internal/mcp/        mcp stdio server, tool table, harness install
internal/output/     json/jsonl/pretty/toon renderers, error envelope, exit codes
internal/config/     config.toml, credentials.toml, xdg paths
internal/cli/skills/ embedded agent docs
docs/                this spec, ADRs
```
