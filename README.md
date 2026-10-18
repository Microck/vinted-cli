<p align="center">
  <img src=".github/assets/vinted-cli-logo.svg" width="720" alt="vinted cli logo">
</p>

<p align="center">
  <a href="https://github.com/Microck/vinted-cli/releases"><img src="https://img.shields.io/github/v/release/Microck/vinted-cli?display_name=tag&style=flat-square&label=release&color=000000" alt="release badge"></a>
  <a href="https://www.npmjs.com/package/vinted-cli"><img src="https://img.shields.io/npm/dt/vinted-cli?style=flat-square&label=downloads&color=000000" alt="npm downloads"></a>
  <a href="https://github.com/Microck/vinted-cli/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/Microck/vinted-cli/ci.yml?branch=main&style=flat-square&label=ci&color=000000" alt="ci badge"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-mit-000000?style=flat-square" alt="license badge"></a>
</p>

---

`vinted` is a terminal cli for vinted that lets you search the catalog with the filters the web has, watch searches, items, and members for changes, and chat with sellers, all from your own account. it compiles to one static Go binary. output is json by default so everything pipes into `jq`. `--format pretty` is for humans.

[documentation](https://vinted.micr.dev/docs) | [command demos](https://vinted.micr.dev/docs/examples) | [npm](https://www.npmjs.com/package/vinted-cli) | [github](https://github.com/Microck/vinted-cli)

![search demo](images/demos/search.gif)

## why

if you already use vinted and want it in the terminal, this cli gives you a practical path without the browser. search once and keep the results as a stream of events, talk to sellers from your own account, and filter by catalog, brand, size, condition and colour, all from one static binary with no daemon. output is json by default, so scripts, agents, and shell pipelines all read the same thing.

## quickstart

### linux or macos

install the binary with curl:

```bash
curl -fsSL https://vinted.micr.dev/install.sh | sh
vinted --version
```

the installer places `vinted` in `~/.local/bin` (override with `VINTED_INSTALL_DIR`).

### windows

install via scoop:

```powershell
scoop bucket add microck https://github.com/Microck/scoop-bucket
scoop install vinted
vinted --version
```

### using a package manager

```bash
# homebrew
brew install Microck/tap/vinted

# npm
npm install -g vinted-cli

# arch linux (aur)
yay -S vinted-cli-bin
```

### go install

```bash
go install github.com/Microck/vinted-cli/cmd/vinted@latest
vinted --version
```

every release ships `checksums.txt` with a github provenance attestation:

```sh
gh release download --repo Microck/vinted-cli --pattern checksums.txt
gh attestation verify checksums.txt --repo Microck/vinted-cli
```

check that the install works:

```bash
vinted --version
vinted doctor
```

`doctor` checks that the config loads, every sink is valid, the proxy setting parses, no cooldown is running, the vinted site answers, the session works, and the state of the watch schedule.

### auth

vinted keeps your login in browser cookies, so the cli imports them instead of asking for a password. email, google, apple, and facebook accounts all work the same way.

1. log in on your vinted domain in your browser (www.vinted.com, www.vinted.fr, www.vinted.es, ...)
2. export cookies for that site with an extension such as cookie-editor (netscape `cookies.txt` or json; a whole-browser export works too)
3. run `vinted auth login` and paste the file path, or pass `--cookies FILE`

```bash
# interactive prompt (paste a cookie file path or a cookie header)
vinted auth login

# import from an exported cookie file
vinted auth login --cookies ~/Downloads/cookies.txt

# pick the vinted domain explicitly (default: the host the cookies came from)
vinted auth login --cookies cookies.txt --domain vinted.fr

# read from stdin
cat cookies.txt | vinted auth login --cookies-stdin
```

the cli needs `refresh_token_web` from the export (and normally `access_token_web` with it). the cookies go into the bundled [galleton](https://github.com/Microck/galleton) session engine, an encrypted vault next to `credentials.toml`, and never into the cli itself. the engine renews the access token through vinted's own refresh endpoint before it expires, keeps the rotated cookies, and is the only thing that ever spends the single-use refresh token. after importing, log out of that browser session (or just log in again there): the browser still holds the refresh token the engine spends. vinted can still expire or revoke a session, so you may need to log in again.

check session status or refresh by hand:

```bash
vinted auth status            # local, offline
vinted auth status --check    # asks the engine and vinted
vinted auth refresh           # renew now
```

the engine starts with the first command that needs the account and stops ten seconds after the last one. to keep it renewing between commands (and after a reboot, once you log in), run `vinted auth service enable`; `vinted auth service status / disable / stop / run` manage it. details: [docs/galleton.md](docs/galleton.md).

run multiple accounts side by side using profiles:

```bash
vinted auth login --cookies alt.txt --profile alt
vinted profile list
vinted profile use alt
vinted search "levis 501" --profile alt
```

upgrading from a version before the session engine: run `vinted auth login` once with a fresh export. older versions kept the cookies in `credentials.toml`; the new one never reads them and drops them on its first write.

## updates

```bash
vinted update
vinted update --check
vinted update --yes
```

eligible interactive commands check for a new stable release at most once a day and ask `Update? [Y/n]`. `auth`, `watch`, `completion`, `help`, and `update` are excluded from automatic checks. scripts, MCP, and `--no-input` never prompt or check automatically. set `VINTED_NO_UPDATE_CHECK=1` to disable automatic checks. npm, Homebrew, Scoop, and AUR installs update through their package manager; direct binaries use checksum-verified release archives. development and prerelease builds are not replaced.

## auth model

| credential | where it comes from | what it unlocks |
|---|---|---|
| none | | `search`, `item show`, `user *`, `category list`, `brand search`, watches on public data |
| session cookies | browser export via `auth login`, held by the session engine | favorites, own wardrobe, saved searches, chat, offers, managing your own items, `me *` |

vinted gates some features behind its own account verification: chat (reading the inbox too), offers, reservations, favorites and every change to your own items answer code 115 until vinted has verified the account. the cli turns that into exit `3` with a link to the verification page. search, item and member pages, saved searches and watches work without it.

## staying unblocked

vinted sits behind datadome and cloudflare. they watch for traffic that looks automated, and when they decide yours is, they block the session ("Se ha bloqueado tu sesión", unusual activity) and sometimes the whole ip for hours. a blocked session can take your browser login down with it. the cli is built to stay well under that line, and it cannot be talked out of it:

- **one request every 2 seconds per site, shared by every process.** all `vinted` processes and profiles on the machine queue behind one another through a small state file (`$XDG_STATE_HOME/vinted-cli/throttle.json`, locked while it is updated), with up to 1s of random jitter on every gap. raise it with `vinted config set request_interval 5s`; it cannot go below 1s.
- **a block stops everything.** a captcha page, a datadome or cloudflare block, an http 429, or an unexplained 403 ends the command with exit `5`, is never retried, and starts a 6 hour cooldown (longer if vinted's `Retry-After` asks for it). until it ends, every command fails at once with the time left and sends nothing, and the session engine is paused for as long, so it does not renew the session meanwhile. `vinted cooldown` shows it, `vinted doctor` and `vinted auth status` too. `vinted cooldown clear` lifts both early; only do that once vinted works normally in your browser.
- **small requests only.** `search --pages` is capped at 5, a search watch reads at most 5 pages and a seller watch at most 10 wardrobe pages per check, and one command (one watch tick, one mcp tool call) may send at most 60 requests.
- **slow schedules.** `watch add`, `watch run` and `watch service install` refuse intervals under 15 minutes; scheduled runs start after a random delay of up to a tenth of the interval, and watches are checked one after another. `chat open` polls every 30s (floor 10s).
- **no needless requests.** the csrf token and the guest session are cached in the same state file (0600), so a command does not reload the home page each time; one keep-alive connection serves a whole command; every request carries the same chrome-on-linux headers, with client hints that match the user agent and the site's language.

what you should do:

- keep intervals generous. a few watches every 15-30 minutes is fine; dozens every 15 minutes is not.
- run the cli from the same network you browse vinted from. datacenter, vps and commercial vpn ip ranges are flagged far faster than a home connection.
- if you need a proxy, set `proxy` in config.toml or `VINTED_PROXY` (`socks5h://127.0.0.1:1080`, `http://host:port`). all vinted traffic then goes through it, the session engine's token renewals included, and if it is unreachable the request fails with exit `6`; it never falls back to a direct connection. a running engine keeps the proxy it started with, and a command with a different one refuses to use it. config.toml is world-readable, so a proxy with credentials goes in `VINTED_PROXY` only (and then cannot be baked into `watch service install` or `auth service enable`).
- when you hit a block: stop, wait out the cooldown, open vinted in your normal browser and solve any captcha there. if the session stays blocked, export fresh cookies and run `vinted auth login` again.

none of this makes automated use invisible. vinted can still block the session; the cli only makes it much less likely and stops making it worse when it happens.

## command surface

| command | what it does |
|---|---|
| `auth login / status / refresh / logout` | import, inspect, refresh or forget a session |
| `auth service enable / disable / status / run / stop` | keep the session engine renewing between commands |
| `profile list / use / remove` | several accounts side by side |
| `search [text]` | catalog search with every web filter: `--catalog`, `--brand`, `--size`, `--condition`, `--color`, `--material`, `--pattern`, price and `--currency`, the category ones (`--video-game-rating`, `--platform`, `--model`, `--storage`, `--sim-lock`, `--language`), `--attr CODE=ID` for anything else; `--format jsonl` prints the result page on one line (`jq -c '.items[]'` for one item per line) |
| `search filters` | the filters and option ids vinted offers for a search or `--catalog` (sizes, conditions, colours, materials, patterns...) |
| `category list` | catalog ids |
| `brand search` | brand ids for `--brand` |
| `item show / images / open / favorite / unfavorite` | inspect an item by id or url: every photo, details, shipping, buyer protection, category path and seller box (`--seller` adds the public profile); `images --download DIR` saves the photos |
| `item create / edit / delete / hide / unhide` | manage your own items: publish with photos (`--draft` saves a draft), change fields, remove (`--yes`), hide |
| `user show / items / reviews` | look at a member by id or url |
| `me show / items / favorites` | your account |
| `alert list` | the account's saved searches, read-only; copy one with `watch add search --from-alert ID` |
| `chat list / show / send / start / open` | messaging; `open` is a line-mode chat that polls for new messages |
| `chat offer / offer accept / offer decline` | send a price offer, or answer the latest pending one as the seller |
| `chat reserve` | reserve your item for the buyer in a conversation (`--off` clears it) |
| `watch add search\|item\|seller` | create a watch (baselines silently, `--emit-initial` to report what exists; `search --from-alert ID` copies a saved search) |
| `watch check / run / events / list / remove` | one-shot check, foreground loop (15m floor), history |
| `watch service install / uninstall / status` | systemd user timer or launchd agent running `watch check` |
| `sink list / test` | notification targets from config |
| `config path / get / set / list` | config.toml |
| `doctor` | checks config, sinks, proxy, cooldown, site, session, schedule |
| `cooldown` / `cooldown clear` | show or lift the pause after vinted blocked a request |
| `skills list / get` | embedded docs for agents |
| `mcp` / `mcp install` | serve the cli to an agent harness over MCP |
| `update` | update the binary |
| `completion bash\|zsh\|fish\|powershell` | shell completion |

global flags: `--profile`, `--format json|jsonl|pretty|toon`, `--no-color`, `--error-format text|json`, `--no-input`, `--debug`.

## shell completion

```bash
# zsh
vinted completion zsh > "${fpath[1]}/_vinted"

# bash
vinted completion bash > /etc/bash_completion.d/vinted

# fish
vinted completion fish > ~/.config/fish/completions/vinted.fish

# powershell
vinted completion powershell >> $PROFILE
```

## examples

```bash
vinted search "levis 501" --price-to 30 --order newest_first --format pretty
vinted brand search nike
vinted search "air max" --brand 53 --size 208 --condition very_good --limit 20
vinted search filters --catalog 1845 --format pretty
vinted search --catalog 1845 --pattern 21 --color 9 --material 303 --price-to 40 --currency USD
vinted item show https://www.vinted.com/items/4923018331-levis-501 --seller --format pretty
vinted item images 4923018331 --download ./photos --limit 3

vinted watch add search "levis 501" --price-to 30 --name levis --notify phone
vinted watch check --all --format jsonl | jq -r 'select(.type=="item.price_changed") | .item.url'
vinted watch service install --interval 30m

vinted chat list --format pretty
vinted chat start 4923018331 "hi, is this still available?"
echo "would you take 20?" | vinted chat send 18273645 -
vinted chat offer 18273645 20

vinted alert list --format pretty
vinted watch add search --from-alert 4650918777 --name saved

vinted item create --title "levi's 501" --description "worn twice" --price 25 \
  --category 1840 --condition "very good" --size "W32" --brand "levi's" --color blue \
  --image front.jpg --image back.jpg
vinted item edit 4923018331 --price 20
vinted item hide 4923018331
vinted item delete 4923018331 --yes
```

## what it looks like

these recordings use public listings. the watch writes only to its isolated local demo database. see the [command demo gallery](https://vinted.micr.dev/docs/examples) for search, filters, item details, and watch events.

### inspect a listing

![item demo](images/demos/item.gif)

### discover and apply filters

![filters demo](images/demos/filters.gif)

### track changes with a watch

watches save a search locally; each check reports what changed.

![watch demo](images/demos/watch.gif)

## sinks config

a sink is a few lines of config:

```toml
[sinks.phone]
type = "ntfy"
url = "https://ntfy.sh"
topic = "vinted-deals"

[sinks.discord]
type = "webhook"
url = "https://discord.com/api/webhooks/..."
template = "discord"

[sinks.script]
type = "exec"
command = ["~/bin/on-vinted-event"]   # event json on stdin
```

## exit codes

`0` ok, `1` generic, `2` usage (including a refused interval, `--pages` over the cap, or a command past its 60-request budget), `3` auth, `4` not found, `5` blocked by vinted's bot protection or inside the cooldown that follows (do not retry; see [staying unblocked](#staying-unblocked)), `6` network (including an unreachable proxy), `7` vinted changed its api or pages. stderr carries a prefilled issue link. `130` interrupted.

## building from source

```bash
git clone https://github.com/Microck/vinted-cli && cd vinted-cli
make build     # bin/vinted
make check     # vet, staticcheck, tests, gofmt
```

## documentation

full docs live at [vinted.micr.dev/docs](https://vinted.micr.dev/docs). install, the cookie export walkthrough, the command reference generated from the binary, exit codes, sinks, and scheduling. the site source is in `docs-site`.

- [docs/cli-spec.md](docs/cli-spec.md): the interface contract and every endpoint the cli uses
- [AGENTS.md](AGENTS.md): vocabulary
- [docs/adr](docs/adr): decisions

## contributing

issues and pull requests are welcome. when vinted changes something, the cli prints a prefilled issue link. that is the most useful report you can file.

## disclaimer

this is an unofficial client. vinted has no public api for members, and its terms and conditions prohibit automated access, scraping, and using the platform other than through its own interfaces. the stated remedy is restricting or closing the account. use it with an account you are willing to lose, keep the check interval reasonable, and do not use it to spam sellers. not affiliated with vinted.

## license

[mit license](LICENSE)
