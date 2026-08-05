# junctera frontend

Next.js (App Router) dashboard for the [junctera](../../README.md) B2B SaaS template — auth, organizations + switcher, members, RBAC roles, audit logs, subscription. TypeScript + Tailwind v4 + [shadcn/ui](https://ui.shadcn.com) (`base-nova` preset) + [TanStack Query](https://tanstack.com/query).

## Running it

Requires the backend running first — see the [root README](../../README.md) (`make up && make migrate && make seed && make api`).

```bash
nvm use        # optional — reads .nvmrc (Node 24)
pnpm install
pnpm dev       # http://localhost:4000
```

The Node major is declared in three places that must agree: `.nvmrc` (local shells and CI, which reads it via `node-version-file`), the `NODE_VERSION` build `ARG` in `Dockerfile`, and `engines.node` in `package.json`. Change one, change all three.

`next dev` runs on **:4000**, not the framework default :3000 — the Go API already owns :3000, and both need to be up at once during development.

Copy `.env.local.example` → `.env.local` to override `BACKEND_URL` (defaults to `http://localhost:3000`, correct for local dev against `make api`).

## How the browser talks to the API

The browser **never** calls the Go API directly — it only ever calls same-origin `/api/*`. `app/api/[...path]/route.ts` is a runtime reverse proxy (a Next.js Route Handler) that forwards each request to `BACKEND_URL`.

This is deliberately **not** a `next.config.ts` `rewrites()` entry. `next.config.ts` is evaluated once during `next build` and its resolved output (including a `rewrites()` destination) is baked into the standalone server's manifest — a container-runtime env var set *after* the build (e.g. `docker compose`'s `BACKEND_URL=http://api:3000`) would never take effect. A Route Handler reads `process.env.BACKEND_URL` fresh on every request instead, so the same built image works unmodified across environments (local dev, compose, a future k8s deploy) — see [Next's own docs on runtime environment variables](https://nextjs.org/docs/app/guides/environment-variables#runtime-environment-variables) for the pattern this follows.

Same-origin-only also means **no CORS configuration exists on the backend** — it was never needed.

## Auth / token model

- **Access token**: held in memory only (a module-level variable in `lib/auth/token-store.ts`), never persisted. Lost on every full page reload by design.
- **Refresh token**: persisted in `localStorage`. On mount, `SessionProvider` (`lib/auth/use-session.tsx`) uses it to silently re-authenticate if there's no in-memory access token yet.
- **Single-flight refresh**: the API client (`lib/api/client.ts`) catches a `401`, and if it's not itself the refresh call, single-flights a shared `/auth/refresh` call across any concurrent requests, then retries the original request once. A failed refresh clears all tokens and the app falls back to anonymous.
- **Active org**: `lib/org/active-org.ts` tracks the selected organization id (also in `localStorage`, under a separate key) via a small pub-sub in `token-store.ts` + `useSyncExternalStore`, so every component reading it (nav gating, the org switcher, org-scoped queries) stays in sync without prop drilling. Selecting a different org invalidates every org-scoped TanStack Query.
- **`localStorage` is shared across tabs on the same origin** — opening a second tab does not give you a second, independent session; it inherits whatever refresh token is currently stored. Worth knowing when testing multi-user flows locally.

## Pages

| Route | Notes |
| --- | --- |
| `/login`, `/register` | `(auth)` route group; redirects to `/organizations` if already authed |
| `/organizations` | list + create (dialog) + switch active org |
| `/members` | roster, invite, remove — invite/remove disabled for `member`-role callers (mirrors the backend's own check) |
| `/roles` | RBAC: create role, edit permissions (textarea, one per line), assign to a member |
| `/audit` | filterable by action / user / limit |
| `/subscription` | current plan + limits, assign a plan (via `GET /plans`, a Phase-6-only backend addition — see root `docs/03-target-architecture.md`) |

All dashboard routes live under the `(dashboard)` route group, whose layout is the auth guard: redirects anonymous callers to `/login`, shows a skeleton while the session is resolving.

## Design system

Every primary noun in this product is an identifier with a grammar — a slug (`northwind`), a permission (`billing:*`), an audit action (`org.member.invited`), a role tier. The UI treats them as data rather than prose, which is why mono carries most of the weight.

**Type** (three roles, `app/layout.tsx`): Martian Mono (`--font-display`) for the wordmark and page titles; IBM Plex Sans (`--font-sans`) for prose, labels, and controls; IBM Plex Mono (`--font-mono`) for identifiers, timestamps, and metadata. Display face is reserved for section nouns — verb phrases like "Log in" stay in sans.

**Color** (`app/globals.css`): a cool blue-slate palette. `--signal` (violet) means *elevated authority* and nothing else — it is deliberately **not** the primary button color, so violet in the interface always reads as "broad privilege". `--wire` (desaturated cyan) marks the admin tier. Dark mode is a deep blue-slate, not near-black.

**Theming**: `next-themes` (`app/providers.tsx`) with `attribute="class"`, which is what the `dark` variant in `globals.css` keys off. `components/theme-toggle.tsx` offers light / dark / system as a radio group — three mutually exclusive states, not a cycling button, since `system` is the default and would otherwise be unreachable. It reads the hydration state through `useSyncExternalStore` (same shape as `lib/org/active-org.ts`) rather than a `useState` + `useEffect` mount flag, which the `react-hooks/set-state-in-effect` lint rule rejects. `<html>` carries `suppressHydrationWarning` because next-themes sets the class in a pre-hydration script.

**Components that encode product semantics** (not just styling):

| Component | What it encodes |
| --- | --- |
| `components/scope-chain.tsx` | Identity › tenant › authority — the backend's `RequireAuth → RequireOrg → RequirePermission` chain made visible. Doubles as the org switcher, and is the app's only animated element: a sweep fires when the active tenant changes, the one state transition where acting on the wrong org is costly. |
| `components/role-badge.tsx` | Authority tier, encoded twice (color *and* marker fill: disc / ring / none) so it survives greyscale and color blindness. |
| `components/permission-token.tsx` | The `*` > `resource:verb` > `resource:*` precedence. Wildcards carry `--signal`, so scanning a role's grants shows violet exactly where authority is broad. |
| `components/data-table.tsx` | The shared table shell — column rhythm and rule weight live here so they can't drift between the five table pages. |

Nav in `(dashboard)/layout.tsx` is grouped **account** vs **tenant**, mirroring the backend's own middleware split: tenant routes grey out with a dashed marker when no org is selected, because those requests would be refused without `x-organization-id`.

The scope chain distinguishes three states, not two: no tenant selected (`unscoped`), a tenant selected whose membership is still loading (`resolving…`), and resolved. Collapsing the middle case into "unscoped" would contradict the nav and misreport scope. It also drops the identity segment entirely when the email is unknown — `/auth/refresh` mints a `sub`-only access token by contract (`docs/02`), so after any full reload the email genuinely isn't available client-side.

## Commands

```bash
pnpm dev                 # dev server, :4000
pnpm build                # production build (runs typecheck as part of the build)
pnpm exec tsc --noEmit    # typecheck only
pnpm test                 # vitest (lib/api/client.test.ts — single-flight refresh coverage)
pnpm lint                 # eslint
```

## Docker

```bash
docker build -t junctera-web:dev .
```

Standalone output (`output: "standalone"` in `next.config.ts`). The runner sets `HOSTNAME=0.0.0.0` explicitly — without it the standalone server binds to the container's assigned network IP rather than all interfaces, which breaks the loopback-based `HEALTHCHECK` even though external port-forwarding still works. See the root `compose.yaml`'s `web` service for how this is wired into the full stack.
