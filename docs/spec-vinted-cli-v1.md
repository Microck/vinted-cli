# vinted-cli v1

Vocabulary: `AGENTS.md`. Decisions already recorded: `docs/adr/0001-session-from-web-cookie.md` (its storage and refresh were later replaced by the bundled session engine, `docs/adr/0002-cli-managed-galleton.md`). Interface contract: `docs/cli-spec.md`.

## Problem Statement

Vinted is a browser-and-app-only marketplace. Someone hunting for a specific second-hand piece has to reopen the site, re-enter the same Search, scroll past what they already saw, and remember which prices dropped. Talking to a seller means switching to the web inbox. Nothing can be scripted, scheduled, piped into other tools, or driven by an agent, and there is no public API.

## Solution

A single-binary Go CLI, `vinted`, that logs in with the user's own account by importing the browser Session, and then exposes the web's buyer-side features as noun-verb commands with JSON output by default: Search with the Filters the catalog offers, Item and Member inspection, Favorites, the account's own wardrobe, Conversations and Messages, and local Watches that turn the marketplace into a stream of Events delivered to stdout or Sinks. Background Checks run from the OS scheduler. When Vinted changes its API or its pages, the CLI fails loudly with a prefilled GitHub issue link instead of degrading.

## User Stories

### Authentication and profiles

1. As a buyer, I want to log in by exporting cookies from my browser, so that email, Google, Apple and Facebook accounts all work the same way.
2. As a buyer, I want the CLI to refresh my Access token on its own and persist what comes back, so that I do not log in again every hour.
3. As a buyer, I want to paste a cookie export path or a cookie header into a prompt, so that I do not have to remember flags.
4. As a script author, I want to pass the cookie export with a flag or on stdin, so that login works without a terminal.
5. As a buyer, I want the CLI to validate the Session at login and tell me who I am logged in as, so that a bad export fails immediately.
6. As a buyer on vinted.fr or vinted.es, I want the Profile to remember my domain, so that searches, prices and links match the site I use.
7. As a buyer with two accounts, I want named Profiles, so that each account has its own Session, domain and Watches.
8. As a buyer, I want to see the state of my Session, so that I know whether it is still valid before running a batch.
9. As a CI user, I want to supply the cookies by environment variable, so that a job can run without a credentials file.
10. As a privacy-conscious user, I want the credentials file kept separate from config at mode 0600, so that sharing my config never leaks my Session.

### Search

11. As a buyer, I want to search by text with price, Catalog, Brand, size, condition and colour filters, so that the common web filters are one flag away.
12. As a buyer, I want to sort by newest or price, so that I can spot fresh or cheap listings first.
13. As a buyer, I want to look up Brand ids and the Catalog tree, so that I can pick the right `--brand` and `--catalog`.
14. As a buyer, I want a raw `--filter key=value` passthrough, so that a catalog parameter without a flag still works.
15. As a buyer, I want to page through results, so that deep searches are possible.

### Items, members, account

16. As a buyer, I want the full detail of an Item including the total price with buyer protection and the reserved and sold flags, so that I know whether it is worth messaging about.
17. As a buyer, I want to pass an Item URL copied from the browser as well as an id.
18. As a buyer, I want to open an Item in my browser and list its image URLs.
19. As a buyer, I want to add and remove Favorites, so that my web favorites stay in sync.
20. As a buyer, I want to see a Member's profile, rating, reviews and wardrobe, so that I can judge whether to buy from them.
21. As a seller, I want to list my own wardrobe and my Favorites.

### Chat

22. As a buyer, I want to list my Conversations with unread state and last Message.
23. As a buyer, I want to read a Conversation oldest-first.
24. As a buyer, I want to send a Message, with the text from an argument or stdin.
25. As a buyer, I want to message a seller about an Item I have no Conversation with yet, reusing an existing one if there is one.
26. As a buyer with an unverified account, I want a clear error when Vinted requires verification before using the inbox.

### Watches, Checks, Events, Sinks

27. As a buyer, I want to save a Search as a Watch, so that new listings find me.
28. As a buyer, I want to watch a single Item for price changes, reservation, sale or removal.
29. As a buyer, I want to watch a Member for new listings.
30. As a buyer, I want the first Check to record a baseline silently, with an option to emit everything instead.
31. As a buyer, I want a one-shot Check for cron or a systemd timer, and a foreground loop with a floored, jittered interval.
32. As a buyer, I want Events as JSON objects delivered to ntfy, a webhook or my own executable, configured once and attached to Watches by name.
33. As a buyer, I want to test a Sink with a synthetic Event, and a failed Sink not to block state being saved.
34. As a buyer, I want the Event history stored and queryable.
35. As a buyer, I want to install, inspect and remove a background schedule with one command each.

### Output, errors, configuration

36. As a script author, I want JSON on stdout by default and everything else on stderr.
37. As a human, I want `--format pretty` tables that respect `NO_COLOR`, `TERM=dumb` and non-TTY.
38. As an agent author, I want `--format toon` and `--format jsonl`, embedded skill docs and an MCP server.
39. As a script author, I want distinct exit codes and a JSON error envelope.
40. As a user, I want API-changed errors to carry a prefilled, redacted issue link that never contains my search text.
41. As a user, I want a debug flag that logs every request with secrets redacted.
42. As a user, I want a doctor command that checks config, Session, reachability and schedule state.

### Distribution

43. As a user, I want `go install`, a `curl | sh` installer, Homebrew, Scoop, npm and AUR.
44. As a security-minded user, I want release checksums with GitHub attestations.
45. As a reader, I want a README disclaimer that this is unofficial and against Vinted's terms.

## Implementation Decisions

- Stack: Go, cobra with pflag, `net/http` only, `modernc.org/sqlite`, `pelletier/go-toml/v2`, `adrg/xdg`, goreleaser. Single static binary.
- One package, `internal/vinted`, owns every path, header, page parser and raw shape and normalizes them into the stable output contract (Item, User, Review, Category, Brand, Conversation, Message). Upstream drift is fixed there.
- Session model (ADR 0001): the stored secret is the imported cookie jar. The Access token is the `access_token_web` JWT; it is refreshed through `POST /web/api/auth/refresh` 60 seconds before its `exp`, and the returned cookies are persisted.
- Search and item detail parse the server-rendered catalog and item pages, because Vinted serves no JSON for them on the web. Everything else uses `/api/v2`.
- Item and Member references are numeric ids or URLs; the id is extracted from the URL.
- Watch semantics, delivery order, scheduling, Sinks, output and error envelopes carry over from the shape documented in `cli-spec.md`.

## Testing Decisions

Tests drive the CLI through its in-process entrypoint and assert on exit codes, stdout, stderr, files on disk and requests received by a fake HTTP server serving redacted recordings. No mocks of Go interfaces. Page parsing is tested against saved, redacted catalog and item pages.

## Out of Scope

- Email and password login.
- Checkout, payments, shipping, marking Items sold outside the order flow, settings edits. (Managing the account's own Items, Drafts, Offers, Reservations and reading Saved searches were added after v1 for parity with wallapop-cli; see `cli-spec.md`.)
- Reading browser cookie stores directly.
- Any anti-bot evasion beyond sending a browser user agent.

## Further Notes

- Vinted's terms prohibit automated access and scraping. The README disclaimer states this and that restricting or closing the account is the stated remedy.
