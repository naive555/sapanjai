#!/usr/bin/env bash
# Run a task on the local model (aider -> LM Studio) inside an isolated git
# worktree, then report the diff and every check the repo already defines.
#
# The worktree is the whole point: a local model that misunderstands the task
# produces a branch to delete, not a mess on dev.
#
# No `set -e`: a failing check is a result to report, not a reason to abort.
set -uo pipefail

REPO="$(git rev-parse --show-toplevel 2>/dev/null)" || { echo "not in a git repo" >&2; exit 1; }
export PATH="$HOME/.local/bin:$HOME/.lmstudio/bin:$HOME/go/bin:/opt/homebrew/bin:$PATH"

LMS_URL="http://localhost:1234"
BASE="HEAD"; SLUG="task"; MSG=""; EDIT_FORMAT=""; MODEL=""; KEEP=0; FILES=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -m|--message)      MSG="$2"; shift 2 ;;
    -M|--message-file) MSG="$(cat "$2")"; shift 2 ;;
    -n|--name)         SLUG="$2"; shift 2 ;;
    -b|--base)         BASE="$2"; shift 2 ;;
    -e|--edit-format)  EDIT_FORMAT="$2"; shift 2 ;;
    --model)           MODEL="$2"; shift 2 ;;
    --keep)            KEEP=1; shift ;;
    -f|--files)        shift; while [[ $# -gt 0 && "$1" != -* ]]; do FILES+=("$1"); shift; done ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

[[ -z "$MSG" ]]          && { echo "need -m/--message or -M/--message-file" >&2; exit 2; }
[[ ${#FILES[@]} -eq 0 ]] && { echo "need -f/--files (aider only edits files you name)" >&2; exit 2; }
SLUG="$(printf '%s' "$SLUG" | tr -cs 'a-zA-Z0-9' '-' | sed 's/^-//;s/-$//' | cut -c1-40)"
[[ -z "$SLUG" ]] && SLUG="task"

# ---- preflight -------------------------------------------------------------
command -v aider >/dev/null || { echo "FAIL: aider not on PATH" >&2; exit 1; }

LMS_JSON="$(mktemp)"
curl -s -m 5 "$LMS_URL/api/v1/models" -o "$LMS_JSON" 2>/dev/null
LOADED="$(python3 - "$LMS_JSON" <<'PY'
import json, sys
try:
    doc = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
for m in doc.get("models", []):
    for inst in m.get("loaded_instances", []):
        cfg = inst.get("config", {})
        print("%s\t%s\t%s" % (m.get("key", "?"),
                              cfg.get("context_length", "?"),
                              cfg.get("parallel", "?")))
PY
)"
rm -f "$LMS_JSON"

if [[ -z "$LOADED" ]]; then
  echo "FAIL: no model loaded in LM Studio at $LMS_URL" >&2
  echo "  start the server (Developer tab), then:" >&2
  echo "  lms load qwen2.5-coder-14b-instruct-mlx --context-length 16384 --parallel 1 -y" >&2
  exit 1
fi
echo "### local model"
printf 'model\tctx\tparallel\n%s\n' "$LOADED"

# ---- worktree --------------------------------------------------------------
STAMP="$(date +%H%M%S)"
BRANCH="delegate/${SLUG}-${STAMP}"
WT_ROOT="${TMPDIR:-/tmp/}"; WT_ROOT="${WT_ROOT%/}/sapanjai-delegate"
WT="${WT_ROOT}/${SLUG}-${STAMP}"
mkdir -p "$WT_ROOT"
git -C "$REPO" worktree add -q -b "$BRANCH" "$WT" "$BASE" || { echo "FAIL: worktree create" >&2; exit 1; }
BASE_SHA="$(git -C "$WT" rev-parse HEAD)"
echo; echo "### worktree"; echo "$WT  (branch $BRANCH, base ${BASE_SHA:0:8})"

discard() {
  git -C "$REPO" worktree remove --force "$WT" >/dev/null 2>&1
  git -C "$REPO" branch -D "$BRANCH" >/dev/null 2>&1
}

# A worktree starts without node_modules and the frontend can't typecheck
# without it. Symlink rather than reinstall: ~500MB, identical to the checkout.
if [[ -d "$REPO/apps/frontend/node_modules" && ! -e "$WT/apps/frontend/node_modules" ]]; then
  ln -s "$REPO/apps/frontend/node_modules" "$WT/apps/frontend/node_modules"
fi

# ---- run the local model ---------------------------------------------------
AIDER_ARGS=(--yes-always --analytics-disable --no-check-update --no-show-release-notes --no-stream)
[[ -n "$EDIT_FORMAT" ]] && AIDER_ARGS+=(--edit-format "$EDIT_FORMAT")
[[ -n "$MODEL" ]]       && AIDER_ARGS+=(--model "$MODEL")

echo; echo "### aider"
cd "$WT" || exit 1
START=$(date +%s)
aider "${FILES[@]}" --message "$MSG" "${AIDER_ARGS[@]}" 2>&1 | tail -25
echo "($(( $(date +%s) - START ))s)"

if [[ "$(git -C "$WT" rev-parse HEAD)" == "$BASE_SHA" ]]; then
  echo; echo "### result: NO CHANGES — the model committed nothing."
  [[ $KEEP -eq 0 ]] && discard
  exit 3
fi

# ---- checks ----------------------------------------------------------------
CHANGED="$(git -C "$WT" diff --name-only "$BASE_SHA" HEAD)"
echo; echo "### changed files"; echo "$CHANGED"

FAILED=0
run() { # run <label> <dir> <cmd...>
  local label="$1" dir="$2"; shift 2
  local out rc
  out="$(cd "$dir" && "$@" 2>&1)"; rc=$?
  if [[ $rc -eq 0 ]]; then
    echo "PASS  $label"
  else
    FAILED=1; echo "FAIL  $label"; printf '%s\n' "$out" | head -25 | sed 's/^/      /'
  fi
}

echo; echo "### checks"
if printf '%s\n' "$CHANGED" | grep -q '\.go$'; then
  # gofmt -l exits 0 even when it lists unformatted files, so test its output.
  UNFMT="$(cd "$WT/apps/backend" && gofmt -l . 2>&1)"
  if [[ -n "$UNFMT" ]]; then
    FAILED=1; echo "FAIL  gofmt"; printf '%s\n' "$UNFMT" | sed 's/^/      /'
  else
    echo "PASS  gofmt"
  fi
  run "go build"  "$WT/apps/backend" go build ./...
  run "go vet"    "$WT/apps/backend" go vet ./...
  command -v golangci-lint >/dev/null && run "golangci-lint" "$WT/apps/backend" golangci-lint run
  # Integration tests t.Skip without DATABASE_URL/REDIS_URL, and a worktree has
  # no .env, so this is the unit-test subset by construction.
  run "go test"   "$WT/apps/backend" go test ./...
fi
if printf '%s\n' "$CHANGED" | grep -qE '\.tsx?$'; then
  run "tsc"  "$WT/apps/frontend" pnpm exec tsc --noEmit
  run "lint" "$WT/apps/frontend" pnpm lint
fi

echo; echo "### diff"
git -C "$WT" diff "$BASE_SHA" HEAD

echo; echo "### verdict"
[[ $FAILED -eq 0 ]] && echo "all checks PASSED" || echo "checks FAILED — review before merging"
echo "worktree: $WT"
echo "branch:   $BRANCH"
echo "merge:    git -C $REPO cherry-pick ${BASE_SHA:0:8}..$BRANCH"
echo "discard:  git -C $REPO worktree remove --force $WT && git -C $REPO branch -D $BRANCH"
exit $FAILED
