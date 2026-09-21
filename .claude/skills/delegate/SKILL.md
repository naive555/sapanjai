---
name: delegate
description: Hand a narrow, mechanical coding task to the local model (aider + LM Studio) in an isolated git worktree, then review the diff and check results. Use when the user asks to delegate, offload, or hand work to the local/small model, or says "let the local model do it". Not for architectural or security-sensitive work.
---

# Delegate to the local model

You stay the orchestrator. The local model types; you specify, review, and decide.
It never touches the working tree — `delegate.sh` runs it in a throwaway worktree
on its own branch.

## Decide whether to delegate at all

The local model is Qwen2.5-Coder-14B at 4-bit in a 16k window. It does not hold
this repo's constraints — the fail-open quota lookup, lease-based outbox claiming,
additive-forward migrations, "no CORS middleware", explicit field-by-field admin
DTOs. It will violate them confidently.

**Delegate:** table-driven test case expansion · swaggo annotation sweeps ·
field-by-field DTO mappers from a named sqlc row · repetitive handler/service
boilerplate that mirrors an existing file · mechanical renames across many files ·
frontend form scaffolds copied from an existing form.

**Do not delegate:** anything in `internal/module/admin` (the leak-prevention
invariants are the point) · envelope encryption or key handling · auth, session
rotation, RBAC semantics · migrations · anything where `docs/02-api-contract.md`
is the source of truth · anything you cannot state as a mechanical transformation.

If a task needs a paragraph of *why* to get right, write it yourself. The round
trip plus your review will cost more than typing it.

## Write the spec

`delegate.sh` passes your message straight through, so it is the entire brief.
Include, every time:

- The **exact transformation**, stated mechanically.
- The **files it may touch** (via `-f`), and an explicit "do not modify X" for
  anything adjacent it might wander into.
- **Acceptance criteria a compiler or test can check** — not "handle errors
  properly" but "return `apperror.ErrNotFound` when the row is missing".
- A **pattern file to imitate** when one exists. Naming the file it should copy
  is worth more than three sentences of description.

Do not paste CLAUDE.md in. It will not survive 16k tokens alongside the source.

## Run it

```
.claude/skills/delegate/delegate.sh \
  -n add-usage-tests \
  -f apps/backend/internal/module/billing/usage_test.go \
  -m "<the spec>"
```

| flag | |
|---|---|
| `-n` | branch slug (`delegate/<slug>-<time>`) |
| `-f` | files aider may edit — **it edits only these** |
| `-m` / `-M` | spec inline / from a file (use `-M` for long specs) |
| `-e diff` | for files over ~400 lines; default `whole` is more reliable but sends the entire file both ways |
| `--model` | e.g. `openai/qwen2.5-coder-7b-instruct-mlx`, ~3x faster for trivial sweeps |
| `--keep` | keep the worktree even if nothing changed |

Exit codes: `0` all checks passed · `1` a check failed · `3` model committed nothing.

## Review

The check results are necessary, not sufficient. **Read the diff yourself** — the
script prints it. Passing `go test` only means it did not break what was already
tested.

Look for: invented helpers that duplicate existing ones · changes outside the
stated scope · a test asserting the implementation rather than the behaviour ·
comments narrating *what* the code does (this repo's ground rule is *why*) ·
error strings that do not match `docs/02-api-contract.md`.

Report to the user: what it did, what the checks said, what you found reading it,
and your recommendation. Attribute failures accurately — if the spec was wrong,
say the spec was wrong.

Then merge or discard with the commands the script prints. Never merge without
showing the user the diff first.

## When the model is not loaded

The script fails preflight with the fix. Memory on this machine is tight (16GB);
`parallel: 1` at ctx 16384 is what makes the 14B fit — LM Studio's default of 4
allocates 4x the KV cache and the load gets refused.
