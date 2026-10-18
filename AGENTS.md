# vinted-cli

Terminal client for the Vinted marketplace: search and inspect listings, track them over time, and talk to sellers, all against Vinted's unofficial web API and server-rendered pages.

## Language

### Marketplace

**Item**:
A single listing on Vinted, identified by its numeric id. Item URLs (`/items/{id}-slug`) resolve to that id; the slug is decoration.
_Avoid_: Listing, product, ad, post

**Member**:
A Vinted account as seen from outside: the seller of an Item, or the subject of `user show`. The CLI's noun for it is **User**; Vinted's URLs say `member`. Identified by a numeric id.
_Avoid_: Owner, vendor, seller (only when talking about an Item's Member)

**Catalog**:
Vinted's own category tree for Items (women, men, kids, home, ...). A Catalog id narrows a Search; `category list` prints the tree.
_Avoid_: Category id (the flag is `--catalog`)

**Brand**:
A named brand in Vinted's brand table, identified by a numeric id. `brand search` finds the id for `--brand`.

**Search**:
A text query plus a set of Filters against Vinted's catalog page.
_Avoid_: Query (that is only the text part)

**Filter**:
One constraint on a Search: price range, Catalog, Brand, size, condition, colour, material, or a raw catalog parameter passed with `--filter`.

### Tracking

**Watch**:
A saved instruction to observe one target (a Search, an Item, or a Member's wardrobe) and report changes. Lives locally, never on Vinted's servers.
_Avoid_: Alert (that is Vinted's Saved search, the `alert` noun), tracker, monitor, subscription

**Check**:
One run of a Watch: fetch the current state, compare it with the last stored state, emit Events, store the new state. Idempotent.
_Avoid_: Poll, tick, scan

**Event**:
One observed change produced by a Check: new Item, price changed, reserved, sold, removed, Member listed something.
_Avoid_: Notification (that is the delivery, not the change), alert (a Saved search)

**Sink**:
A destination an Event is delivered to besides stdout: ntfy, webhook, or an executable.
_Avoid_: Notifier, channel, target

**Saved search**:
A Search the account saved on Vinted, which Vinted itself alerts about. Server-side and distinct from a Watch. The CLI exposes it as the `alert` noun (`alert list`, `watch add search --from-alert`).

### Account

**Profile**:
One named Vinted account known to the CLI, with its Session and Vinted domain. One Profile is the default.
_Avoid_: Account (ambiguous with the Vinted account itself), user

**Session**:
The imported browser cookies that let the CLI act as a Profile's account: `access_token_web`, `refresh_token_web` and whatever else the export carried. Stored in `credentials.toml` at mode 0600.
_Avoid_: Credentials, login

**Access token**:
The short-lived JWT in `access_token_web` (about an hour), sent on every authenticated request. Refreshed automatically before it expires.
_Avoid_: Session token, bearer

**Refresh token**:
The `refresh_token_web` cookie. Exchanged at `POST /web/api/auth/refresh` for a new Access token; whatever comes back is persisted.
_Avoid_: Session

**Favorite**:
An Item the account has hearted on Vinted. Server-side, unlike a Watch.

**Draft**:
An Item the account started in the upload form but did not publish. Only the account sees it; it has its own id.

### Chat

**Conversation**:
A Vinted inbox thread between the account and one other Member, usually about one Item. Identified by a numeric id.
_Avoid_: Chat, thread, room

**Message**:
One text entry inside a Conversation, sent by either side.

**Offer**:
A price proposal inside a Conversation's transaction: an offer request from the buyer, or a counter offer from the seller. The seller accepts or declines a pending offer request.

**Reservation**:
The seller's promise of an Item to the buyer of one Conversation. Set from the Conversation, never on the Item alone.
