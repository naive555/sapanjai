# Security Policy

Sapanjai exists to sit between an AI agent and a customer's systems and refuse the calls
that shouldn't happen. A vulnerability here is not an inconvenience — it is the product
failing at the one thing it is for. Reports are genuinely welcome.

## Reporting a vulnerability

**Please do not open a public issue, pull request, or discussion for a security problem.**

Use one of these instead:

1. **GitHub private vulnerability reporting** — preferred. On this repository, go to the
   **Security** tab → **Report a vulnerability**. The thread is private between you and the
   maintainer, and it can become a published advisory with credit once a fix ships.
2. **Email** — `non.naive@gmail.com`, with `SECURITY` in the subject line.

Helpful to include, to whatever extent you have it: what an attacker gains, the steps or
request sequence to reproduce, the commit or deployment you observed it on, and whether
you believe it is already being exploited.

Sapanjai is maintained by one person. You will get an acknowledgement as soon as it is
seen, and an honest assessment — including "this will take a while" or "I disagree that
this is a vulnerability, here's why" — rather than a service-level promise that cannot be
kept. If a week passes with no reply at all, please send a second message; assume the
first was lost rather than ignored.

## Coordinated disclosure

Please give a reasonable window to ship a fix before publishing — 90 days is a fine
default, shorter if the issue is being actively exploited, and negotiable in either
direction. Reporters are credited in the advisory and the release notes unless they ask
not to be.

Good-faith research under this policy is welcome, and no legal action will be pursued over
it. That covers testing against **your own** self-hosted instance. It does not extend to
testing against the hosted service without arranging it first, to accessing or exfiltrating
another tenant's data, to degrading availability for other users, or to social engineering
anyone.

## What is in scope

The parts of this codebase where a flaw is most serious, roughly in order:

- **Tenant isolation.** Any path by which one organization reads or affects another
  organization's connectors, keys, members, audit log, or upstream data. This is the
  highest-severity class in the product.
- **The MCP gateway** (`internal/module/mcp`). A tool reachable by a principal whose RBAC
  grant does not permit it; a `tools/call` that succeeds after the permission was revoked;
  a connector allowlist that can be escaped; a signed file-download link that can be
  forged, extended, or replayed against a different connector.
- **MCP keys** (`internal/module/mcpkey`). A revoked, expired, or forged
  `sk_live_…` token being accepted; a key's `scopes` widening rather than narrowing its
  creator's grant.
- **Authentication and sessions** (`internal/module/auth`). Refresh-token reuse that does
  not revoke the family, a bypass of the login rate limit, a verification or reset token
  that can be redeemed twice or guessed, an access token that survives logout.
- **Secrets at rest and in transit.** Any route, log line, error, or audit record that
  exposes a decrypted connector `config`, a `password_hash`, an MCP key hash, or a live
  token from an email body. A weakness in the envelope encryption in
  `internal/shared/envelope`.
- **The staff console** (`internal/module/admin`). Privilege escalation into or within
  platform staff, anything that lets a `support` role perform a superadmin mutation, an
  impersonation token that can write, or a bypass of the IP allowlist or TOTP step-up.
- **RBAC** (`internal/module/rbac`). Permission matching that grants more than
  `*` > `resource:verb` > `resource:*` should.

## What is out of scope

- **`spikes/`.** Throwaway feasibility modules, deliberately outside the build and not
  dependencies of anything shipped. Findings there are not vulnerabilities.
- **Self-hosted misconfiguration** — a weak `JWT_ACCESS_SECRET`, a reused
  `CONNECTOR_MASTER_KEY`, `ADMIN_REQUIRE_2FA=false`, an empty `ADMIN_IP_ALLOWLIST`, a Redis
  or Postgres exposed to the internet. If the documentation *invites* one of these, though,
  that is a real report and worth sending.
- **Behavior that is documented as deliberate**, unless you can show the reasoning is
  wrong. Several designs that look like bugs are argued for in `docs/` and in `CLAUDE.md` —
  email verification being a banner rather than a gate, `forgot-password` always returning
  200, the `banned:` Redis cache having no TTL, access tokens outliving a ban or a password
  reset until their own ≤15-minute expiry. A demonstration that one of these is exploitable
  in a way the reasoning missed is very much in scope.
- Missing hardening headers, absent rate limits on unauthenticated read routes, and
  scanner output with no demonstrated impact.
- Denial of service through sheer volume against a self-hosted instance.

## Supported versions

Sapanjai is pre-1.0 and ships from `main`. Fixes land there, and only there — there are no
maintained release branches to backport to. If you self-host, track `main`.
