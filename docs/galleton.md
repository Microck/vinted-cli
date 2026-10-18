# The session engine

The normal workflow is one command:

```sh
vinted auth login --cookies ~/Downloads/cookies.txt
vinted me show
```

Release binaries contain a pinned [Galleton](https://github.com/Microck/galleton) daemon
(v0.1.0) for their OS and architecture. It holds the account's cookies in an encrypted vault,
renews the access token through Vinted's refresh endpoint, keeps the cookies every response
sets, and sends every account request with them attached. The CLI process never holds an
account cookie, so no two processes can ever spend the same single-use `refresh_token_web`.
No separate installation, configuration file, port or environment variable is needed. The
design record is [ADR 0002](adr/0002-cli-managed-galleton.md).

## Installation and source builds

The shell installer, Homebrew, Scoop, npm and AUR all ship the same self-contained release
binary. `make build` and `make install` embed the engine too. GoReleaser builds it for Linux,
macOS and Windows on amd64 and arm64 before embedding the matching one in each release.

A plain `go install github.com/Microck/vinted-cli/cmd/vinted@VERSION` cannot run the bundle
generator. On its first logged-in use it builds and caches the same pinned module with the Go
toolchain it was installed with, after checking both module checksums, so it needs module
download access once. No executable is ever fetched from `latest`, `PATH` or a URL. The pin
and checksums live in `internal/galleton/bundle/version.go`.

## Login

`vinted auth login` parses the export with Galleton's `cookiefile` package: Netscape
`cookies.txt`, Cookie-Editor, EditThisCookie, DevTools and Playwright JSON lists (bare or
wrapped as `{"url": ..., "cookies": [...]}`), or a `Cookie:` header. A whole-browser export is
narrowed to the Vinted sites while parsing, and then to the origins of the profile's site. The
cookies are imported into the engine as a new session, beside the profile's current one, under
an ID derived from the credentials file and the profile name plus a random part. The engine
renews right away, the CLI reads the account through it, and only then does `credentials.toml`
get the new session ID, the account id and the login, and the replaced session is deleted. A
rejected import is removed from the vault again and the profile keeps its previous session.

After the import the browser still holds the refresh token the engine just spent. Log out
there (and log in again if you want the website) so the browser gets its own session.

## Lifecycle

Commands load config and credentials without starting anything. The first request that needs
the account takes a process lease and starts, or reuses, one engine per state directory. The
engine binds an OS-picked loopback port and reports it; the CLI checks its API token and health
answer before using it. Separate commands, watches and the MCP server share it. Ten seconds
after the last client exits, the engine drains and stops; the next command starts it again on
the same vault.

Watch checks attach to the engine once per run, so a scheduled watch timer keeps a stored
session renewed even when the watches themselves only read public pages.

## Unattended renewal

Nothing is installed as a service automatically. To keep the engine renewing between commands
and start it again at OS login:

```sh
vinted auth service enable
vinted auth service status
vinted auth service disable
```

This writes a systemd user unit on Linux, a LaunchAgent on macOS, or a logon task on Windows,
with absolute paths and the `HOME`, `XDG_*`, `VINTED_CONFIG`, `VINTED_BASE_URL`,
`VINTED_PROXY` and `HTTP(S)_PROXY`/`NO_PROXY` of the shell that enabled it. It never contains a
credential: enable refuses a proxy variable carrying one. A Windows logon task cannot carry variables; it starts with the
saved user environment, so enable refuses when one of those variables differs there (save it
with `setx` first). Linux user lingering is not enabled, so this is a login service,
not renewal while the machine is off.

For a container or another supervisor, run `vinted auth service run` in the foreground.
`vinted auth service stop` stops an idle engine and refuses while clients are still attached.

## Egress, pacing and blocks

The engine's adapter is generated, one provider per Vinted site: `vinted` for www.vinted.com
(the same shape as galleton's `examples/adapters.vinted.json`), `vinted-fr`, `vinted-es`, and
so on, plus `vinted-custom` for a `VINTED_BASE_URL`.

- **Proxy.** `VINTED_PROXY`, else the config `proxy`, reaches the engine through `proxy_env`,
  so a proxy with credentials never lands in a file. Renewals and account requests use it and
  never fall back to a direct connection. An engine records the adapter and proxy it started
  with; a command configured differently refuses to attach until it is stopped.
- **Renewal.** `POST /web/api/auth/refresh` with an empty JSON body and the web app's own
  headers. Both `access_token_web` and `refresh_token_web` must come back in `Set-Cookie`, and
  `expires_in` schedules the next renewal five minutes before expiry. Only a renewal may set
  those two cookies: a page that hands out guest tokens cannot replace them.
- **Cookie scope.** www.vinted.com shares the tokens with its gateway api.www.vinted.com
  through `.www.vinted.com`; national sites share theirs with `api.vinted.xx` through
  `.vinted.xx`.
- **Blocks.** A Datadome or Cloudflare challenge, or a 403 that is not a Vinted JSON error,
  during a renewal is a temporary refusal: the engine backs off and keeps the credentials.
  When a CLI request is blocked, the CLI starts its cooldown and pauses the engine's provider
  for as long, so no renewal goes out meanwhile; the pause survives an engine restart.
  `vinted cooldown clear` lifts both.
- **Pacing.** The CLI's request gate (one request per two seconds per site, shared by every
  process) stays in front of every engine call, and the engine spaces its own calls, renewals
  included, by at least a second.
- **Size.** Requests up to 33 MiB (a 32 MiB photo upload and its multipart envelope) and
  responses up to 16 MiB (catalog pages are about 4 MiB). Connections are reused for account
  requests; renewals always use a fresh one.

## Storage

The vault lives in `galleton/` next to `credentials.toml`, under
`$XDG_CONFIG_HOME/vinted-cli`. The directory is private (0700), files are 0600, the extracted
engine is 0700. The local API token and encryption key there administer the whole vault, and
encryption does not protect against other processes running as the same user: do not share
the directory or put it in public backups. On Windows the user profile's ACLs have to protect
it.

`auth status` and `profile list` stay offline and show `source: "galleton"` and the session
ID. `auth status --check` asks the engine for its state (status, last and next renewal, access
token expiry) and reads the account. `auth refresh` renews now and fails nonzero when Vinted
refuses. `auth logout` and `profile remove` delete the engine's session before the local
reference; this does not log the browser out.

## Upgrading from an older vinted-cli

Older versions kept the cookie jar in `credentials.toml` and refreshed it themselves. This
version never reads those cookies: such a profile shows `needs_login`, every account command
asks for `vinted auth login`, and the next write of `credentials.toml` removes the old cookies.
Import a fresh browser export once. `VINTED_COOKIES` is gone too; it would have needed the old
in-process refresh.

## External daemon

`VINTED_GALLETON_DIR`, `VINTED_GALLETON_URL` or `VINTED_GALLETON_TOKEN_FILE` point the CLI at
a daemon you run yourself. The CLI then neither starts nor stops it, and its adapter has to
name the providers as above (galleton's `examples/adapters.vinted.json` covers
www.vinted.com). Normal use leaves these unset.

## Verification

Tests run the real pinned engine: the CLI suite drives every logged-in command through it
against a fake Vinted that rotates its single-use refresh token in `Set-Cookie`, and checks
that blocks pause the engine, that a different proxy is refused, that the vault holds no
plain-text cookie and that the proxy carries the renewals. A TLS test checks rotation across
an engine restart. No Vinted account is used in CI.
