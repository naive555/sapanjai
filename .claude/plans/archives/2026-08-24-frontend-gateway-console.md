# Frontend — from platform console to gateway console

> **Status: complete (planned 2026-08-24, finished 2026-08-24).** All 5 phases
> shipped.
>
> - [x] Phase 1 — Foundation: `font-heading`, nav re-grouping, thesis copy (`e70237c`)
> - [x] Phase 2 — Activity: typed gateway rows, the eight missing actions (`0c94ce6`)
> - [x] Phase 3 — Connector detail: the Span, a permanent endpoint home (`cfbaa77`)
> - [x] Phase 4 — Backend: `since` + repeatable `action` on `/audit-logs` (`8a9d1c6`)
> - [x] Phase 5 — Overview: the Span as the front door *(in the working tree,
>       not yet committed as of archiving)*
>
> **Read first:** `CLAUDE.md`, `docs/02-api-contract.md`, `docs/05-mcp-gateway.md`,
> `docs/07-sheets-adapter-decisions.md`, `apps/frontend/app/globals.css` (the palette
> rationale comment at the top is the design brief this plan extends).
>
> **Decisions already taken by the owner (do not re-litigate):**
> 1. **Keep and extend** the existing visual identity. No re-skin.
> 2. **Primary user: the technical admin at the customer** — IT-capable, wires up the
>    connector, hands keys to colleagues. Comfortable with IDs and scopes; not a Go
>    developer. Keep the precision, drop the middleware vocabulary.
> 3. **Frontend + small backend additions** are in scope.
>
> **Execution rule:** this plan is phased, and each phase is an approval gate. Stop
> after each phase, report what landed, and wait — approving one phase never approves
> the next.
>
> **Archived 2026-08-24, once the last phase shipped.** The maintained state of this
> work lives in the code and in `docs/02-api-contract.md` (the `/audit-logs` params
> and the recorded-actions list) — those are the files to update when it changes
> again, not this one.

---

## 1. Diagnosis

The dashboard is a well-executed admin console **for a platform template**. The product
is now a **managed MCP gateway**. Every screen is a CRUD table over a resource; no
screen is about the *connection*, which is the thing the customer actually bought.

Five concrete gaps, each verified in the code:

### A. Gateway traffic is invisible, though the backend records it in detail

`internal/module/mcp/service.go` writes five audit actions with genuinely rich
metadata — `tool`, `duration_ms`, `row_count`, `missing_permission`, `connector_id`,
`spreadsheet_id`, `filter_columns`. The dashboard reads none of it:

- `app/(dashboard)/audit/page.tsx`'s `KNOWN_ACTIONS` lists 7 actions and omits **all
  five `mcp.*` actions and all three `connector.*` actions**. They cannot even be
  filtered. The list mirrors `docs/02-api-contract.md` line 114, which was never
  updated when the MCP actions were added further down the same document.
- The metadata cell is `max-w-xs truncate` rendering `key=value` pairs, so
  `tool=sheets_query_rows duration_ms=412 row_count=88` is cut off mid-value.

`mcp.tool.denied` is the single most valuable debugging signal in the product —
denied tools are *invisible* to the agent by design (`docs/05` §"Two enforcement
layers"), so a denial is the only evidence a user will ever get that something was
filtered rather than broken. It currently has no surface at all.

### B. The connect flow is a scavenger hunt, and its best artifact is write-once

Getting an agent working today: read the 426-line setup guide → create a connector
(paste OAuth credentials) → run a health check → go to `/mcp-keys` → mint a key →
**the `claude mcp add` command appears exactly once, inside a dialog that deliberately
cannot be reopened** (`mcp-keys/page.tsx`, the reveal `Dialog` with the no-op
`onOpenChange`) → and the connector ID it needs lives in a table on a different page.

The once-only *token* is correct and must stay. The once-only *instructions* are not:
after that dialog closes, **no screen in the app tells you your endpoint URL.** The
in-code comment argues a docs-page copy "would have a placeholder where the token is,
which is exactly the step people get wrong" — but the step people actually get wrong
is finding the endpoint at all, and the placeholder version is the one a user can
return to.

### C. The nav is shaped like the backend's middleware, not the user's task

`NAV_GROUPS` in `app/(dashboard)/layout.tsx` is grouped `account` / `tenant`, with a
comment stating it mirrors the `RequireAuth` / `RequireOrg` split. That is honest and
the disabled-when-unscoped behaviour it drives is genuinely good — but "tenant" is
system vocabulary. The admin's mental model is: *what's connected · who can reach it ·
what happened · what it costs.*

### D. The product thesis is absent

`app/page.tsx` is a redirect-only skeleton. The auth tagline is "Tenants, roles, and
an audit trail for all of it" — the **template's** pitch, predating the gateway pivot.
Nothing in the UI says this product stands between an agent and a system of record.

### E. The second tier of the type system is dead code

`font-heading` is applied in five places in the CSS — `components/ui/dialog.tsx:125`
(so **all 11 dialog titles**), `components/ui/card.tsx:41`, and three headings in the
Sheets setup guide — but there is no `--font-heading` token and no `@utility
font-heading` in `globals.css`. Tailwind emits nothing for an undefined `font-*`
utility, so **14 live headings** silently fall back to the body sans at default
tracking. (`CardTitle` itself currently has zero usages, so the card half of the bug is
latent rather than visible — it will bite the moment Phase 3 or 5 uses a card.) The
display face survives only in the four hand-written `font-display` spots.

---

## 2. Design direction — extend the grammar that is already here

No new palette, no new typefaces. The system already contains an unfinished idea worth
completing.

### The connector vocabulary already exists

`ScopeChain`'s `<Separator connected>` draws a **solid hairline when scope resolves and
a dashed one when the chain is broken**; the nav reuses the same dashed rule for
routes you cannot reach. That is a visual grammar for *"does this link hold?"* — and it
is currently used for exactly three segments in one header.

`ScopeChain` renders the credentials of a **dashboard** request:
`identity → tenant → authority`. A **gateway** request has a longer chain:

```
agent → key → gateway → connector → upstream
```

Drawing that with the same grammar makes the diagnosis self-evident: whichever span is
dashed is the thing to go fix.

### `--wire` is the missing traffic colour

The palette defines `--signal` (violet, authority) **and `--wire`** (blue) — and `--wire`
is used in exactly one place, the `admin` tier of `RoleBadge`. The token is literally
named for the connection and nothing uses it for one. Assign the three roles explicitly
and hold the line:

| Token | Means | Used for |
| --- | --- | --- |
| `--signal` (violet) | **a permission decided something** | wildcard grants, owner tier, active scope, **and tool denials** — a denial is an authority event, so violet stays truthful |
| `--wire` (blue) | **traffic** | call counts, live spans, throughput, the connected state of a span |
| `--destructive` (red) | **failure** | connector error status, revoked keys, rate-limit exhaustion |

This is additive: no existing use of `--signal` changes meaning, and `--wire` gains the
job its name already implied.

### Signature element — **the Span**

One horizontal, hairline-drawn rendering of a connector's full path. Not a row of
gradient stat cards (the default answer, and explicitly the thing to avoid). ASCII of
the intended composition:

```
   AGENTS            KEY                GATEWAY            CONNECTOR          UPSTREAM
   ┌──────┐          ┌──────┐           ┌──────┐           ┌──────┐           ┌──────┐
   │  2   │──────────│ sk_… │───────────│  ●   │───────────│ acme │╌╌╌╌╌╌╌╌╌╌╌│ ◇    │
   └──────┘          └──────┘           └──────┘           └──────┘           └──────┘
   connected         3 keys             142 calls / 24h    inactive           Google Sheets
                     1 expiring         2 denied           run a health check  4 sheets, 1 folder
```

Rules that keep it a diagnostic rather than a decoration:

- **Every span is solid or dashed, never decorative.** Solid (`--wire`) = this link
  resolves. Dashed (`--border`) = it does not, and the label beneath names the fix.
- **At most one node is ever emphasised** — the leftmost broken one. If the connector
  is inactive, nothing downstream of it lights up, because nothing downstream is
  reachable.
- **The zero state is the onboarding checklist.** With no connector and no key the whole
  span renders dashed, each node labelled with its step. The empty screen is the
  invitation to act, and it is the same component — not a separate empty-state design.
- **Motion:** the existing `scope-sweep` keyframe already fires on tenant change and is
  described in `globals.css` as "the one animated element in the app." The Span reuses
  that exact sweep, and only when a health check flips a connector to active — the one
  other moment a link visibly closes. No new keyframes, no ambient animation.

### Copy direction

Rewrite toward what the admin controls, not how it is built. Concretely:

| Now | Becomes |
| --- | --- |
| "Tenants, roles, and an audit trail for all of it." | "Give an agent a safe door into your systems." |
| nav group `tenant` | `connection` / `access` / `record` |
| "audit log" · "An append-only record of everything that happened here." | "activity" · "Every call an agent made, and everything it was refused." |
| "Upstream connections (Google Sheets, and the generic skeleton) for the MCP gateway." | "The systems an agent can reach through this organization." |

Keep the lowercase mono page titles — that is a deliberate idiom and it reads well.

---

## 3. Phases

### Phase 1 — Foundation: fix the type system, re-shape the nav, state the thesis

No new screens, no API work. Cheapest phase, and it touches every page.

1. **Define `font-heading`.** Add `--font-heading` to the `@theme inline` block and an
   `@utility font-heading` in `globals.css`. Point it at `--font-plex-sans` at weight
   600 with tightened tracking — **not** at Martian. Martian is correctly reserved for
   the wordmark and page titles; promoting every card and dialog title to the display
   face would be exactly the over-reach the palette comment warns against. This makes
   the 14 affected headings render as intended for the first time, and stops the bug
   from spreading as later phases add cards.
2. **Re-group the nav** in `app/(dashboard)/layout.tsx`. Keep the `requiresOrg` flag and
   the dashed-disabled treatment verbatim — that behaviour is load-bearing. Change only
   labels and grouping:
   - `Overview` (org) — added in Phase 5; omit the item until then
   - **connection** — `Connectors`, `MCP keys`
   - **access** — `Members`, `Roles`
   - **record** — `Activity`, `Subscription`
   - **account** — `Organizations`
3. **Rewrite the thesis copy**: auth layout tagline, `metadata.description` in
   `app/layout.tsx`, and the `PageHeader` descriptions per the table above.
4. **Make `/` render.** It is a redirect-only `FullPageSkeleton` today. Keep the
   redirect, but have the anon branch land on `/login` with the thesis visible rather
   than a skeleton flash.

**Verify:** `pnpm lint`, `pnpm exec tsc --noEmit`, `pnpm test`. Confirm by eye that card
and dialog titles changed weight/tracking (proving `font-heading` now resolves).

---

### Phase 2 — Activity: make gateway traffic legible

Rename `/audit` → `/activity` (keep a redirect from the old path). Pure frontend, works
against the API as it exists today.

1. **Add the eight missing actions** to `KNOWN_ACTIONS`: `connector.created`,
   `connector.updated`, `connector.deleted`, `mcp.session.started`, `mcp.tool.called`,
   `mcp.tool.denied`, `mcp.ratelimit.hit`, `mcp.file.downloaded`. Group the select by
   namespace (`user.` / `org.` / `role.` / `connector.` / `mcp.`) rather than one flat
   list of fifteen.
2. **Replace the truncated metadata cell with typed rows.** A new
   `components/activity-row.tsx` renders per action instead of dumping `key=value`:
   - `mcp.tool.called` → tool name in the data face, `duration_ms` right-aligned, and
     `row_count` as "· 88 rows" when present. Long calls (>2s) get `--wire` weight.
   - `mcp.tool.denied` → tool name, then the `missing_permission` rendered through the
     **existing `PermissionToken`** component. This is the whole point of the phase:
     the denial reads as a permission grammar the admin can act on, in violet, matching
     the roles page exactly.
   - `mcp.ratelimit.hit` → tool name + "rate limited", `--destructive`.
   - `mcp.file.downloaded` → file id + mime type.
   - `connector.*` → connector name/id.
   - Anything else falls back to today's `key=value` rendering, so a future adapter's
     new action degrades gracefully instead of rendering blank.
3. **Add a "Gateway only" toggle** above the filters — one click to `mcp.*`. Implemented
   client-side until Phase 4 lands multi-action filtering.
4. **Widen the detail column** and drop `truncate` in favour of wrapping; the typed
   renderers are short enough that they fit.

**Verify:** `pnpm test` plus a new test asserting a `mcp.tool.denied` row renders its
`missing_permission` through `PermissionToken`, and that an unknown action still renders.

---

### Phase 3 — Connector detail: give the endpoint a permanent home

Fixes gap B, the worst product hole. Today only `google_sheets` has a detail page.

1. **New `app/(dashboard)/connectors/[id]/page.tsx`** for every connector type. Contents:
   - The **Span** for this one connector (first use of the signature component; build it
     here, reuse it on the overview in Phase 5).
   - **Endpoint** — `POST {gatewayUrl}/mcp/{id}`, via `CopyableCode`.
   - **Wiring snippet**, permanently, with `CONNECTOR_ID` resolved and the token as a
     `<paste your key>` placeholder plus a link to `/mcp-keys`. Keep the existing
     `--header`-must-come-last warning, which is a real trap.
   - **Health**: status, `lastHealthCheckAt`, and the run button with its result inline
     rather than only as a toast.
   - **Recent activity** for this connector — the last ~15 `mcp.*` rows, reusing the
     Phase 2 row renderers, filtered client-side on `connector_id`.
   - For `google_sheets`, a link through to the existing config form (leave that page
     as-is; it is well built).
2. **Resolve the gateway URL** with a new `NEXT_PUBLIC_GATEWAY_URL`, read server-side and
   passed down; fall back to the browser origin with a note that the API host, not the
   dashboard host, is what belongs there. **No backend change needed** — do not add a
   `/config` endpoint for this.
3. **Trim the key-reveal dialog** to what genuinely must be once-only: the token, the
   copy button, the "you will not see this again" warning, and a link to the connector
   page for wiring. The `Callout` about `drive:read` not being implied by `sheets:read`
   moves to the connector page, where it can be re-read.
4. Link connector rows to the new page for **all** types, not just `google_sheets`.

**Verify:** `pnpm test`; manually walk create-connector → detail → copy snippet and
confirm the URL and connector id are correct.

---

### Phase 4 — Backend: two query parameters on `/audit-logs`

The only Go work in this plan. Both additions are additive and backward-compatible.

1. **`since`** (RFC3339 timestamp) — `QueryParams.Since *time.Time`, threaded into the
   existing sqlc query as an optional lower bound. Needed for "last 24 hours" without
   pulling and discarding rows client-side.
2. **Repeatable `action`** — accept `?action=a&action=b`, changing `Action *string` to
   `Actions []string`. A single `action=x` must keep behaving exactly as it does now.
   Needed so "gateway only" is one request rather than five.
3. Add a new goose migration **only if** the query plan needs an index on
   `audit_logs (organization_id, created_at DESC)` — check `EXPLAIN` first; do not add
   one speculatively. Never edit an applied migration.
4. **Update `docs/02-api-contract.md`**: the `/audit-logs` row's params, and the recorded
   actions list on line 114, which is missing the five `mcp.*` actions documented later
   in the same file. Regenerate swagger (`make swagger`).
5. Point the Phase 2 "Gateway only" toggle at the real filter.

**Verify:** `make test`, `make lint`, and an integration test per `CLAUDE.md`'s testing
expectations covering `since`, multi-`action`, and single-`action` back-compatibility.

---

### Phase 5 — Overview: the Span as the front door

The signature screen. Depends on Phase 3's Span component and Phase 4's filters.

1. **New `app/(dashboard)/overview/page.tsx`**, and make it the post-login landing
   (`app/page.tsx` and both layout redirects currently point at `/organizations`).
2. **Hero: the Span**, one per connector, stacked. With no connectors it renders fully
   dashed as the onboarding path — no separate empty-state component.
3. **Beneath it, last 24 hours**, from one `/audit-logs` call: calls, denials,
   rate-limit hits, busiest tool, slowest tool. Rendered as a hairline-ruled row in the
   data face, **not** as gradient stat cards.
4. **"Refused recently"** — the last few `mcp.tool.denied` rows with their
   `PermissionToken`s and a link to the role that would fix each. This is the highest-value
   panel in the product: it is the only place a user learns *why* their agent could not
   see a tool.
5. Add `Overview` to the nav (deferred from Phase 1).

**Verify:** `pnpm test`; check the zero-connector, one-broken-connector, and healthy
states all render, plus dark mode and a 375px viewport.

---

## 4. Explicitly out of scope

- Any re-skin: no new palette, typefaces, radii, or spacing scale.
- Rewriting the Google Sheets setup guide or config form — both are well built.
- Charts or a time-series graph on the overview. The 24-hour row is deliberately
  numeric; a sparkline can come later if the numbers prove insufficient.
- An OAuth consent flow, per-key tool scoping UI, or write tools — all gated on
  backend work listed as not-built in `docs/05` and `docs/07` §3.
- CORS anything. The browser only ever calls same-origin `/api/*`.

## 5. Open questions — settled before Phase 5

Both were put to the owner before Phase 5 was implemented, and both are reflected in
the shipped code.

- **Does the overview need a cross-connector view when an org has one connector?**
  **Settled as recommended:** a single connector renders its Span full-width; two or
  more stack. `app/(dashboard)/overview/page.tsx` needs no branch for this — the Span
  is full-width on its own, so the one-connector case falls out of the same `.map`.
- **Should `mcp.session.started` appear in the activity feed by default?**
  **Settled against the recommendation.** The plan proposed filtering it out of the
  activity feed's default view; the owner scoped that narrower — hide it on the
  **overview only**, and leave `/activity` exactly as it shipped in Phase 2. So the
  overview's 24-hour counts exclude it (a reconnect is not tool traffic) and the Span
  surfaces it as "last seen", while `/activity` still lists session rows by default
  with the `mcp` namespace filter available. Rationale: `/activity` is the
  everything-that-happened record, and silently dropping an action from it would make
  a working agent look idle.
