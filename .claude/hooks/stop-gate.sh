#!/usr/bin/env bash
# Stop: constitution XI — no claiming work done on a tree that does not build. These are the cheap checks
# (about a second with a warm cache), run only for the kinds of file that changed; the full gate is
# `make ci`. Blocks at most once per stop (stop_hook_active), so it can never loop.
set -uo pipefail

input=$(cat)
[ "$(jq -r '.stop_hook_active // false' <<<"$input")" = true ] && exit 0
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0

# Changed paths (tracked and untracked), excluding Claude's own configuration and worktrees.
changed=$(git status --porcelain --untracked-files=all 2>/dev/null | cut -c4- | sed 's/.* -> //' | grep -v '^\.claude/' || true)
[ -n "$changed" ] || exit 0

problems=""
add() { problems+="$1"$'\n'"$(printf '%s\n' "$2" | tail -n 40)"$'\n\n'; }

go_files=$(grep -E '\.go$' <<<"$changed" | while IFS= read -r p; do [ -f "$p" ] && echo "$p"; done)
if [ -n "$go_files" ] || grep -qE '(^|/)go\.(mod|sum)$' <<<"$changed"; then
  if command -v go >/dev/null; then
    if [ -n "$go_files" ] && command -v gofmt >/dev/null; then
      # shellcheck disable=SC2086 # file names in this repo contain no spaces
      unformatted=$(gofmt -l $go_files 2>&1) && [ -z "$unformatted" ] || add "gofmt: not formatted (run gofmt -w):" "$unformatted"
    fi
    # vet with the integration tag compiles every test file too, so a broken integration test shows here.
    if out=$(go build ./... 2>&1); then
      out=$(go vet -tags=integration ./... 2>&1) || add "go vet -tags=integration ./... failed:" "$out"
    else
      add "go build ./... failed:" "$out"
    fi
  fi
fi

# Documents, contracts, schema and config: no retired mechanism described as current, and ROADMAP's task
# counts must equal tasks.md's.
if grep -qE '\.(md|sql|ya?ml|lua|proto)$|(^|/)Makefile$|(^|/)\.env\.example$' <<<"$changed" && [ -x scripts/check-spec-drift.sh ]; then
  out=$(scripts/check-spec-drift.sh 2>&1) || add "scripts/check-spec-drift.sh failed:" "$out"
fi

[ -n "$problems" ] || exit 0
jq -n --arg r "Not done yet — a cheap check failed. Fix it, or say plainly that it is still failing and why:

$problems" '{decision: "block", reason: $r}'
exit 0
