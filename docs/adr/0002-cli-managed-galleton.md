# 0002. The Galleton session engine owns the cookies

Date: 2026-10-18
Status: accepted

## Context

ADR 0001 kept the imported cookie jar in `credentials.toml` and had every process refresh
the access token itself under a file lock. Vinted's `refresh_token_web` is single use:
replaying a spent one revokes the whole login. The lock, the adopt-what-another-process-stored
logic and the staged writes existed only to keep two processes from spending the same token,
and every new caller (a watch timer, the MCP server, a second shell) widened that surface.

[Galleton](https://github.com/Microck/galleton) v0.1.0 is a session-renewal daemon built for
this: it holds the cookies in an encrypted vault, renews them through a configured endpoint,
keeps what each response sets, and relays requests with the cookies attached. It ships a
Vinted example adapter with the provider options this needs: a fail-closed egress proxy,
bot-protection block signatures, a shared `cookie_domain`, protected token cookies, pacing,
a persisted pause, connection reuse and managed request size limits.

## Decision

The CLI uses a pinned Galleton daemon as its session engine, the same way wallapop-cli does:

- Release binaries embed the daemon for their OS and architecture. The CLI extracts it into a
  private directory next to `credentials.toml`, checks its hash, and starts it through a
  same-executable host process on first use. A plain `go install` builds the pinned module
  once with the Go toolchain, after checking both module checksums. One daemon serves every
  process of a state directory; it stops ten seconds after the last one exits, unless
  `vinted auth service enable` keeps it running.
- `credentials.toml` holds only a session ID per profile, plus the account's id and login.
  No cookie ever enters the CLI process: `auth login` parses the export with Galleton's
  `cookiefile` package, narrows it to the site's origins and imports it; every account
  request then goes through the daemon's managed request API. Guest (logged-out) reads keep
  using an anonymous jar minted from the home page, which spends no refresh token.
- The adapter is generated: one provider per Vinted site (`vinted` for www.vinted.com,
  `vinted-fr`, ...), following galleton's `examples/adapters.vinted.json`. Renewal is
  `POST /web/api/auth/refresh` with the web app's own headers, both token cookies are
  protected and must rotate, `expires_in` schedules the next renewal, Datadome and Cloudflare
  challenges are temporary refusals, calls are at least one second apart, and request and
  response limits fit a photo upload and a catalog page.
- `VINTED_PROXY` (or the config proxy) reaches the daemon through `proxy_env`, so renewals
  leave through the same egress as everything else, and never directly. A daemon records the
  adapter and proxy it started with; a command with a different one refuses to attach.
- The CLI's request gate (cooldown, pacing, budget) stays in front of every daemon call.
  When a response is a block, the CLI records its cooldown and also pauses the daemon's
  provider for as long, so no scheduled renewal goes out meanwhile. `cooldown clear` lifts
  both.
- The old refresh, lock and rotation code is gone, and so is `VINTED_COOKIES`. A profile
  saved by an older version is never used; it needs a new `auth login`, and the next write of
  `credentials.toml` drops the old cookies.

This supersedes ADR 0001's storage and refresh implementation, not its choice of importing a
browser session.

## Consequences

- Exactly one party spends the refresh token. The CLI cannot replay a spent one, because it
  never holds one.
- Release binaries are larger, and the engine is built and tested for every release target.
- Renewal happens while some vinted process runs, or all the time with the optional service.
  Nothing renews while the machine is off.
- A crash after Vinted consumed a refresh token but before the daemon saved the answer can
  require a new login. The daemon reports that as `renewal_uncertain` instead of guessing.
- After an import the daemon renews right away, so the browser the cookies came from holds a
  spent refresh token. Logging out there and back in gives it a separate session.
- `--debug` output and errors cannot contain an account cookie: none reaches the process.
