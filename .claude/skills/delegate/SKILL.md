---
name: delegate
description: Hand a narrow, mechanical coding task to the local model (aider + LM Studio) in an isolated git worktree, then review the diff and check results. Use when the user asks to delegate, offload, or hand work to the local/small model, or says "let the local model do it". Not for architectural or security-sensitive work.
---

# Delegate to the local model

You stay the orchestrator. The local model types; you specify, review, and decide.
It never touches the working tree — `delegate.sh` runs it in a throwaway worktree
on its own branch.

## Decide whether to delegate at all

The local model is Qwen2.5-Coder-7B at 4-bit in an 8k window. It does not hold
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

**Size ceiling — this machine has 16GB.** `edit-format: whole` regenerates the
entire file, so editing a 112-line file means ~200 lines of output: a long
generation with the KV cache growing throughout, while `go build`, `golangci-lint`
and `go test` fork compilers alongside it. That combination put this machine into
swap, and swapping model weights on unified memory freezes the desktop rather
than merely slowing it.

So: **creating a new file is the cheap case and the one to prefer.** Do not
delegate an edit to an existing file over ~150 lines — write those yourself, or
delegate a new file and wire it up by hand. `delegate.sh` refuses to start below
25% free memory, but that guard only catches the obvious cases; the shape of the
task is the real control.

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
| `--model` | e.g. `openai/qwen2.5-coder-14b-instruct-mlx` — needs 7.75GB resident, so only with Docker quit and nothing else running |
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

The script fails preflight with the fix:

```
lms load qwen2.5-coder-7b-instruct-mlx --context-length 8192 --parallel 1 --ttl 900 -y
```

`--parallel 1` matters: LM Studio defaults to 4, which allocates 4x the KV cache
and gets the load refused outright. `--ttl 900` unloads the model after 15 idle
minutes so it stops holding ~4.3GB between delegations.

Quit Docker Desktop before a run. On 16GB the two compete directly, and the
failure mode is a frozen desktop, not a slow one.
