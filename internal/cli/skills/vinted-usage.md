# vinted-cli usage

`vinted` is a terminal client for Vinted. Output is JSON on stdout by default; `--format pretty` for humans, `--format jsonl` to stream, `--format toon` for compact agent context. Errors go to stderr; `--error-format json` gives a machine-readable envelope.

## Exit codes

| code | meaning |
|---|---|
| 0 | success (including "nothing changed") |
| 1 | generic failure |
| 2 | usage: bad flags, bad id or URL, cancelled prompt |
| 3 | auth: no session, session rejected or account verification required (`vinted auth login`) |
| 4 | not found |
| 5 | blocked by Vinted's bot protection, or inside the cooldown after a block; do NOT retry, stop and tell the user (`vinted cooldown` shows the time left) |
| 6 | network or timeout |
| 7 | Vinted changed its API or pages; stderr has a prefilled issue link |
| 130 | interrupted |

## Staying unblocked

Vinted's bot protection blocks sessions that look automated, and a blocked session can take the user's browser login with it. The CLI paces every request (one per 2s per site, shared by all processes), caps work per command (`--pages` ≤ 5, 60 requests per command or tool call), refuses watch intervals under 15m, and after any block refuses all requests for 6h (exit 5, no network). As an agent: ask for what you need in as few commands as possible, never loop or retry on exit 5, never run `vinted cooldown clear` yourself (it is for the user, once Vinted works in their browser), and tell the user to wait, open Vinted in their normal browser, and re-run `vinted auth login` if the block persists.

## Login

```
vinted auth login --cookies cookies.txt [--domain vinted.fr]   # export from a logged-in browser
vinted auth status --check
vinted auth refresh
```

The export must contain `refresh_token_web` (Netscape, Cookie-Editor/DevTools/Playwright JSON, or a Cookie header; a whole-browser export is fine). `auth login` hands the cookies to the bundled Galleton session engine's encrypted vault, never to `credentials.toml`, and the CLI does not keep them; the engine renews the access token itself and is the only party that spends the single-use refresh token. `vinted auth status` is offline; `--check` asks the engine and Vinted. A refused renewal is exit 3: export the cookies again. A profile saved by an older version reports `needs_login`. `vinted auth service enable` keeps the engine renewing between commands; leave that to the user. Profiles: `--profile NAME` or `VINTED_PROFILE`. Never print, log or read cookie values or the `galleton/` directory.

## Search

```
vinted search "levis 501" --price-to 30 --order newest_first --limit 20
vinted search "air max" --brand 53 --size 208 --condition very_good --catalog 1242
vinted search filters --catalog 1845 --format pretty       # filter codes, flags and option ids for a category
vinted search --catalog 1845 --pattern 21 --color 9 --material 303 --price-to 40 --currency USD
vinted search --catalog 3026 --video-game-rating 170 --platform 1273
vinted search --catalog 3661 --attr some_code=1,2           # any other code `search filters` lists
vinted search "wool coat" --filter popular=true            # any other catalog parameter, verbatim
vinted brand search nike                                   # ids for --brand (filters lists only popular ones)
vinted category list                                       # catalog ids for --catalog
```

`--order`: `relevance`, `newest_first`, `price_low_to_high`, `price_high_to_low`. `--condition`: `new_with_tags`, `new_without_tags` (= `new`), `very_good`, `good`, `satisfactory`, `not_fully_functional` (electronics), or an id. Filter flags and codes: `--size` size, `--condition` status, `--color` color, `--material` material, `--pattern` patterns, `--video-game-rating` video_game_rating, `--platform` video_game_platform, `--model` brand_collection, `--storage` internal_memory_capacity, `--sim-lock` sim_lock, `--language` language_book; ids come from `vinted search filters --catalog ID` (options depend on the category). Id flags repeat. Page with `--page N`, or `--pages N` (at most 5) to fetch several. `search filters` costs one request plus one per filter; `--code status --code size` loads only those.

Output: `{"items":[{id,title,price,currency,total_price,brand,size,condition,seller_id,favorites,reserved,sold,images,url,...}],"page":1,"total_pages":...,"total":...}`. `--format jsonl` prints that one document on one line; use `jq -c '.items[]'` for one item per line.

## Items, members, account

```
vinted item show ID_OR_URL [--seller]                    # photos, details, shipping, fees, category path, seller box; --seller adds the profile (1 more request)
vinted item images ID_OR_URL [--download DIR --limit N]  # URLs, or save photos (one request each)
vinted item favorite ID | unfavorite ID
vinted user show USER | items USER | reviews USER        # numeric id or /member/ URL
vinted me show | items | favorites
```

## Chat

```
vinted chat list [--unread] [--limit N]
vinted chat show CONV [--limit N]
vinted chat send CONV "text"          # or: echo text | vinted chat send CONV -
vinted chat start ITEM "text"         # reuses an existing conversation about the item
vinted chat open CONV                 # line-mode chat, polls every --interval (30s, floor 10s)
vinted chat offer CONV 20             # buyer: offer request; seller: counter offer
vinted chat offer accept|decline CONV # seller answers the latest pending offer
vinted chat reserve CONV [--off]      # seller reserves the item for that buyer
```

CONV is the numeric conversation id from `chat list`. Messages are oldest first. Unverified accounts are refused chat, offers, favorites and every change to their own items (exit 3, code 115).

Own items and saved searches:

```
vinted item create --title T --description D --price N --category ID|NAME --condition NAME \
  [--size NAME] [--brand NAME] [--color NAME]... [--attr code=value]... --image FILE... [--draft]
vinted item edit ITEM [--price N] [--title T] ...   # only given flags change
vinted item hide|unhide ITEM
vinted item delete ITEM --yes                       # --draft for a draft id
vinted alert list [--all]                           # saved searches (read-only)
vinted watch add search --from-alert ID --name NAME
```

Field values are checked against what Vinted offers for the category before any upload (exit 2 lists valid values). Publishing (not `--draft`) also needs `--brand` and `--color` for most categories.

## Watches

```
vinted watch add search "levis 501" --price-to 30 --name levis --notify phone   # add --emit-initial to report what exists now
vinted watch add item ID_OR_URL --name jacket
vinted watch add seller USER --name good-seller
vinted watch check [NAME...] [--all]                      # one shot, prints events
vinted watch run [--interval 30m]                         # foreground loop (floor 15m)
vinted watch events [NAME] --since 24h
vinted watch service install|uninstall|status            # systemd/launchd timer
```

Event shape: `{"type":"item.new|item.price_changed|item.reserved|item.unreserved|item.sold|item.removed|seller.new_item","watch":"levis","profile":"default","at":"...","item":{"id":...,...},"change":{"from":30,"to":25}}`.

Sinks are config entries: `[sinks.phone] type="ntfy" url="https://ntfy.sh" topic="deals"`, `type="webhook" url=... template="discord"`, `type="exec" command=["/path/script"]` (event JSON on stdin). `vinted sink test phone` sends a synthetic event.

## MCP

`vinted mcp` is a Model Context Protocol server on stdin/stdout. Each tool runs the command it is named after and returns that command's JSON verbatim; a command that fails comes back as a tool error whose text is the error envelope (`{code,category,retryable,message,...}`), so exit codes above still apply.

```
vinted mcp install claude-code|codex|cursor [--profile NAME]
```

Install writes the server entry into `~/.claude.json`, `~/.codex/config.toml` or `~/.cursor/mcp.json`, leaves every other entry alone, and prints `{harness,path,action,command,args}` with action `added`, `updated` or `unchanged`.

Tools: `search`, `search_filters`, `item_show`, `user_show`, `user_items`, `user_reviews`, `category_list`, `brand_search`, `me_show`, `me_favorites`, `alert_list`, `chat_list`, `chat_show`, `watch_check`. Arguments are the command's flags with dashes as underscores (`price_to`, `sim_lock`); repeatable flags (`catalog`, `attr`, `filter`) take arrays of strings. A bare `mcp install` follows the config's default profile; `--profile`/`VINTED_PROFILE` pins one.

Not tools, on purpose: `chat send/start/open/offer/reserve`, `item favorite/unfavorite` and every change to your own items (`item create/edit/delete/hide/unhide`). Writing to other people or changing the account stays with the person at the terminal.

## Config

`$XDG_CONFIG_HOME/vinted-cli/config.toml`; `vinted config set watch.interval 30m`; `vinted config set request_interval 3s` (floor 1s); `vinted config set proxy socks5h://127.0.0.1:1080` (or env `VINTED_PROXY`; all traffic, never falls back to direct, exit 6 if unreachable); `vinted config path`. The Profile's site is `profiles.NAME.domain` (e.g. `www.vinted.fr`). Session cookies live in the session engine's vault under `$XDG_CONFIG_HOME/vinted-cli/galleton/`; `credentials.toml` (0600) only references them, state in `$XDG_DATA_HOME/vinted-cli/state.db`, the shared cooldown/pacing file in `$XDG_STATE_HOME/vinted-cli/throttle.json`.
