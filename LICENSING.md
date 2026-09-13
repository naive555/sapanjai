# Licensing

Sapanjai is **dual licensed**. The same code is available to you under either:

1. the **GNU Affero General Public License, version 3** (AGPL-3.0-only) — the terms in [`LICENSE`](LICENSE), at no cost; or
2. a **commercial license** from the copyright holder, for anyone who cannot accept the AGPL's terms.

You choose which one you are using. Everything below explains the consequences of that choice.

> This document is a plain-language guide, not legal advice, and it does not modify
> [`LICENSE`](LICENSE). Where the two appear to disagree, `LICENSE` governs. If the
> distinction matters to your organization, have your own counsel read it.

---

## Which applies to you

| What you want to do | What you need |
| --- | --- |
| Read the code, learn from it, fork it, open a pull request | AGPL — nothing to do |
| Run Sapanjai inside your own company, for your own employees | AGPL — nothing to do |
| Modify Sapanjai and keep the changes to yourself, unshipped | AGPL — nothing to do |
| Run Sapanjai (modified or not) as a network service other people use | AGPL — **and you must offer those users the complete source**, see §13 below |
| Ship Sapanjai inside a product you distribute | AGPL — your product must be AGPL too |
| Any of the above, but your policy or your customers forbid AGPL code | **Commercial license** |
| Embed Sapanjai in a closed-source or differently-licensed product | **Commercial license** |
| Use the hosted service at `sapanjai.up.railway.app` | Nothing — the subscription covers it |

## The AGPL, and the one clause that surprises people

The AGPL is the GPL plus **section 13**. Ordinary copyleft licenses are triggered by
*distribution* — handing someone a copy. Section 13 adds a second trigger: letting
people **interact with the software over a network**.

> "Notwithstanding any other provision of this License, if you modify the Program, your
> modified version must prominently offer all users interacting with it remotely through
> a computer network […] an opportunity to receive the Corresponding Source of your
> version." — AGPL-3.0 §13

For Sapanjai that means: if you fork it, change it, and stand the gateway up for other
people to point their MCP clients at, those people are entitled to your modified source.
Not to the data flowing through it — to the code.

Note the precondition: section 13 bites on a version **you modified**. Running an
unmodified Sapanjai as a service does not, by itself, oblige you to publish anything —
though §13's second paragraph still requires you to pass the offer along.

**Satisfying §13 is a link.** A "Source" link in the UI footer pointing at the repository
your build came from is the ordinary way to do it. It must reach *your* version, at the
commit you are running — not this upstream repository, if yours differs from it.

### What the AGPL does *not* do

- It does not reach your customers' data, spreadsheets, or the contents of a connector.
- It does not reach a separate program that merely talks to Sapanjai's HTTP or MCP API.
  Calling a network API is not linking, and does not make your caller a derivative work.
- It does not restrict internal use, however commercial. Copyleft is triggered by
  conveying or by network interaction, not by profit.
- It does not require you to publish a fork you never deploy for others.

## The commercial license

The commercial license exists for one reason: some organizations cannot deploy AGPL code,
whatever the merits, because of an internal policy, a customer contract, or a procurement
rule. It grants the same software under terms without the copyleft obligations —
specifically, without §13's source-offer requirement and without the requirement that
derivative works carry the AGPL.

It is a separate, paid agreement, and it is independent of the hosted subscription: you can
hold one without the other, and you do not need one to be a paying hosted customer.

To ask about one, contact **non.naive@gmail.com** with a sentence about what you are
building and how you intend to deploy it.

## The hosted service is not a licensing question

Subscribing to the hosted Sapanjai buys you an operated service — an endpoint, availability,
upgrades, backups, support. It is a service agreement, not a software license, and its terms
live elsewhere. The distinction matters in both directions:

- **Paying for hosting does not grant you a commercial license** to the code.
- **Not paying for hosting does not take the AGPL away from you.** Self-hosting is a
  first-class, supported, permanently free path, not a trial.

## Contributing

Sapanjai can only be offered under a commercial license if the copyright holder has the
right to license *all* of it that way — including your contribution. A contribution
received under the AGPL alone cannot be relicensed, and a single such commit would make
the commercial license unofferable for the whole project.

So contributions are accepted under a **Contributor License Agreement** — the text is in
[`CLA.md`](CLA.md). You keep your copyright, and you grant the copyright holder a licence
broad enough to include your work in commercially licensed releases. You will be asked to
sign it once, on your first pull request, by replying to a bot; the workflow that enforces
this is [`.github/workflows/cla.yml`](.github/workflows/cla.yml). It changes nothing about
your own freedom to use, fork, or relicense your own code elsewhere.

[`CONTRIBUTING.md`](CONTRIBUTING.md) covers everything else about opening a pull request.

If you would rather not sign, say so on the issue — a description of the fix is often
enough for it to be written independently, and you will still be credited.

## Third-party components

Sapanjai's dependencies remain under their own licenses — MIT, BSD-3-Clause, and
Apache-2.0 across the Go and TypeScript trees. Those are all compatible with AGPL-3.0
distribution, and none of them impose copyleft of their own. The AGPL applies to
Sapanjai's own source, not to the libraries it builds on.

## Copyright

Copyright © 2026 Non Ninkham.

`SPDX-License-Identifier: AGPL-3.0-only`
