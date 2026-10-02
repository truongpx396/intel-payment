#!/usr/bin/env bash
# PreToolUse (Bash): changes here arrive by pull request — main's history is PR merges, and the red-green
# gate reads origin/main..HEAD, so a commit made on main can never be checked. Refuse to commit on the
# default branch or push to it. This guards against an agent's honest mistake with plain git commands;
# it is not a security boundary. To commit on main deliberately, run the command yourself with `!`.
set -uo pipefail

cmd=$(jq -r '.tool_input.command // empty')
[ -n "$cmd" ] || exit 0

deny() {
  jq -n --arg r "$1" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny", permissionDecisionReason: $r}}'
  exit 0
}

branch=$(git -C "${CLAUDE_PROJECT_DIR:-.}" branch --show-current 2>/dev/null || true)
on_default=0
[[ $branch == main || $branch == master ]] && on_default=1

# `git <sub>` at command position (start, or after ; & | or a subshell paren), tolerating global options:
# git -C dir commit, git -c k=v commit. Position matters: `grep "git commit" docs/` is not a commit.
git_sub='(^|[;&|(])[[:space:]]*git([[:space:]]+(-C|-c)[[:space:]]+[^[:space:]]+)*[[:space:]]+'
if [[ $cmd =~ ${git_sub}commit([[:space:]]|$) ]] && [ "$on_default" = 1 ]; then
  deny "On $branch. Create a branch first (git switch -c <type>/<topic>): changes land by pull request, and scripts/verify-red-green.sh can only judge a branch."
fi

if [[ $cmd =~ ${git_sub}push([[:space:]]|$) ]]; then
  # The words of this one command: everything after `git push`, up to the next ; | or &.
  matched=${BASH_REMATCH[0]}
  seg=${cmd#*"$matched"}
  seg=${seg%%[;|&]*}
  names_default=0
  positional=0
  for tok in $seg; do
    case "$tok" in
      main | master | +main | +master | *:main | *:master | *:refs/heads/main | *:refs/heads/master | refs/heads/main | refs/heads/master)
        names_default=1 ;;
      -*) ;;
      *) positional=$((positional + 1)) ;;
    esac
  done
  # `git push` / `git push origin` on main pushes main. (positional: the remote, then any refspecs.)
  if [ "$names_default" = 1 ] || { [ "$on_default" = 1 ] && [ "$positional" -le 1 ]; }; then
    deny "That pushes the default branch. Push a feature branch and open a pull request (ask the user before opening it)."
  fi
fi
exit 0
