#!/usr/bin/env bash
# SessionStart: stdout becomes context. Say only what changes how to work — the branch, and a toolchain
# that would make a gate fail or lie. Silent when everything is as expected.
set -uo pipefail
cd "${CLAUDE_PROJECT_DIR:-.}" || exit 0
git rev-parse --is-inside-work-tree >/dev/null 2>&1 || exit 0

branch=$(git branch --show-current 2>/dev/null)
dirty=$(git status --porcelain 2>/dev/null | grep -vc '^?? \.claude/' || true)
if [[ $branch == main || $branch == master ]]; then
  echo "Branch: $branch — the default branch. Create a branch before committing; changes land by PR (the Bash guard refuses commits and pushes here)."
else
  base=origin/main
  git rev-parse --verify --quiet "$base" >/dev/null || base=main
  counts=$(git rev-list --left-right --count "$base"...HEAD 2>/dev/null | awk '{print $2" ahead, "$1" behind"}')
  echo "Branch: ${branch:-detached} (${counts:-no base found} of $base); $dirty uncommitted path(s)."
fi

warn() { echo "Toolchain: $1"; }
for t in jq golangci-lint go-arch-lint gremlins; do
  command -v "$t" >/dev/null || warn "$t is not on PATH — the hooks or a gate need it (README / docs/testing.md)."
done
if g=$(command -v gremlins); then
  v=$(go version -m "$g" 2>/dev/null | awk '$1 == "mod" {print $3}')
  [ "$v" = v0.6.0 ] || warn "gremlins is ${v:-an unknown version}; CI pins v0.6.0 and scripts/mutation.sh parses its summary — go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0"
fi
if command -v go >/dev/null; then
  gv=$(go env GOVERSION | sed 's/^go//')
  floor=$(awk '$1 == "go" {print $2}' go.mod)
  [ "$(printf '%s\n%s\n' "$floor" "$gv" | sort -V | head -n1)" = "$floor" ] || warn "go $gv is older than go.mod's floor $floor."
fi
if [ ! -S /var/run/docker.sock ] && [ ! -S "$HOME/.docker/run/docker.sock" ]; then
  warn "no Docker socket — integration tests, make ci, verify-hot-path and verify-deploy cannot run."
fi
exit 0
