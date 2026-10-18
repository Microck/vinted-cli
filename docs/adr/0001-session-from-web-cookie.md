# 0001. Sessions come from the web cookies and the refresh endpoint, not from email/password

Date: 2026-10-17
Status: accepted

## Context

Vinted has no public API for members. The web app authenticates with two cookies:
`access_token_web`, a JWT that lives about one to two hours (its `exp` claim says when), and
`refresh_token_web`, HttpOnly. `/api/v2` requests accept the cookies, and also
`Authorization: Bearer <access token>`. When the access token runs out the web app calls
`POST /web/api/auth/refresh` with an empty JSON body and the cookies, and gets back
`{access_token, refresh_token, expires_in: 3600, created_at, ...}`. Datadome and Cloudflare
front every host and set their own cookies.

Alternatives considered:

1. Email + password against Vinted's login form. Login is protected by captcha and Datadome
   challenges, Google/Apple/Facebook accounts cannot use it at all, and the CLI would hold
   the account password.
2. Driving a headless browser through the real login page. Datadome blocks headless browsers,
   and it would drag a browser runtime into a single-binary CLI.
3. Importing the cookies the user already has in their browser and refreshing the access token
   exactly as the web app does. Verified live on 2026-10-17: the refresh call answers 200 with
   a fresh access token for a logged-in cookie set and 400 for a logged-out one.

## Decision

The stored Session is the whole imported cookie jar, keyed by name, plus the access token
expiry. `access_token_web` and `refresh_token_web` are required at import. The client refreshes
60 seconds before `exp`. Every refresh returns a new refresh token (its `iat` changes, the
session id `sid` stays), and replaying a spent one makes Vinted revoke the whole login. So a
refresh runs under the `credentials.toml` lock: the process loads the stored jar, adopts an
access token another process already refreshed instead of refreshing again, and saves the new
jar before unlocking. Other cookie changes merge only the changed entries into the stored jar,
so a long-running process never writes its spent refresh token back, and guest-scope tokens a
page hands out never replace the account's. Cookie import is the only `auth login` path.
`VINTED_COOKIES` supplies a raw cookie header for CI and is never written.

## Consequences

- Login needs a cookie export from a logged-in browser (an extension such as Cookie-Editor),
  because `refresh_token_web` is HttpOnly and cannot be copied from `document.cookie`.
- Refresh is one plain HTTP call, so no background service or daemon is needed: the next
  command that needs a token does it.
- The CLI depends on the refresh route staying put. An unexpected shape or status there
  surfaces as exit 7 with an issue link; a rejected refresh token stays exit 3.
- `credentials.toml` holds live secrets and is written at mode 0600 with locked, atomic
  updates. Cookie and token values are registered for redaction and never printed, not even
  under `--debug`.
- No password ever touches the CLI's storage.
