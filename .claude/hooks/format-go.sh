#!/usr/bin/env bash
# PostToolUse (Edit|Write): keep Go files gofmt-clean — the formatter `make fmt` uses — so a format-only
# diff never shows up in review. A syntax error is reported back to Claude instead of swallowed.
set -uo pipefail

f=$(jq -r '.tool_response.filePath // .tool_input.file_path // empty')
case "$f" in
  */.claude/worktrees/* | */vendor/*) exit 0 ;;
  *.go) ;;
  *) exit 0 ;;
esac
[ -f "$f" ] || exit 0
command -v gofmt >/dev/null || exit 0

if ! out=$(gofmt -l -w "$f" 2>&1); then
  jq -n --arg m "gofmt could not parse $f — the edit left it uncompilable:
$out" '{hookSpecificOutput: {hookEventName: "PostToolUse", additionalContext: $m}}'
fi
exit 0
