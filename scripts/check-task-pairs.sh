#!/usr/bin/env bash
# The task list is itself test-first. tasks.md says every behavioural task is a pair — R### (a failing
# tests-only commit) then T### (the least code that passes it) — and scripts/verify-red-green.sh checks
# the order of the COMMITS. This checks the order of the PLAN, so a list cannot drift away from the rule:
#
#   * every T### has a red above it: the R### of the same number, an R### it names ("makes **R083**
#     green"), `TDD-Exempt: <reason>`, or the words "own red" (a measurement or a check is its own red);
#   * every R### has a green that answers it: a T### of the same number, or one that names it;
#   * a green is never listed before its red, and a name must be a red that exists;
#   * an exemption needs a reason.
#
# Bold defines: `**R083**` at the head of a task line is that task. A T### line may name the red it makes
# green in bold ("makes **R083** green"); anything else that merely refers to another task — a red naming
# another spec's red — is written in plain text, or it would be read as defining it a second time.
#
# Only the tasks between `<!-- pairs:start -->` and `<!-- pairs:end -->` are held to this. Phases that
# predate the rule (001's Phases 0–1) stay outside the markers, and the hardening record says so. A task
# list with no markers is refused, whether it lost them or never had them: an unarmed guard passes everything.
#
#   scripts/check-task-pairs.sh          # run from anywhere; scans specs/*/tasks.md
# Proved able to fail by scripts/verify-tdd-gates.sh (the "task pairing" cases).
set -euo pipefail
cd "$(dirname "$0")/.."
exec python3 -I - specs/*/tasks.md <<'PY'
import re, sys

BOLD = re.compile(r"\*\*([RT]\d{3}[a-z]?)\*\*")
MENTION = re.compile(r"\bR\d{3}[a-z]?\b")
EXEMPT = re.compile(r"TDD-Exempt:")
REASON = re.compile(r"TDD-Exempt:\s*([^`\s.<][^`]*|<(?!reason>)[^`]*)")
OWN_RED = re.compile(r"own red", re.I)
TASK = re.compile(r"^- \[[ x]\] ")

problems, summary = [], []

def problem(path, lineno, msg):
    problems.append(f"PAIRING {path}:{lineno}: {msg}")

for path in sys.argv[1:]:
    lines = open(path, encoding="utf-8").read().split("\n")
    starts = [i for i, l in enumerate(lines) if l.strip() == "<!-- pairs:start -->"]
    ends = [i for i, l in enumerate(lines) if l.strip() == "<!-- pairs:end -->"]
    has_reds = any(re.match(r"^- \[[ x]\] \*\*R\d{3}", l) for l in lines)
    if not starts and not ends:
        why = ("it has R### tasks but no" if has_reds else "it never adopted the rule: no R### tasks and no")
        problems.append(f"PAIRING {path}: the pairing check is not armed — {why} "
                        "`<!-- pairs:start -->` … `<!-- pairs:end -->` region, so nothing would be checked")
        continue
    if len(starts) != 1 or len(ends) != 1 or starts[0] > ends[0]:
        problems.append(f"PAIRING {path}: the pairing check is not armed — it needs exactly one "
                        "`<!-- pairs:start -->` followed by one `<!-- pairs:end -->`")
        continue

    defs = {}      # id -> (lineno, col)
    kinds = {}     # id -> 'R' | 'T'
    text = {}      # id -> its task line
    mentions = []  # (T id, [R names on its line])
    for n in range(starts[0] + 1, ends[0]):
        line = lines[n]
        if not TASK.match(line):
            continue
        ids = [(m.group(1), m.start()) for m in BOLD.finditer(line)]
        if not ids:
            continue
        kind = ids[0][0][0]
        for tid, col in ids:
            if tid[0] != kind:
                continue  # an R### named on a T### line is a reference, not a definition
            if tid in defs:
                problem(path, n + 1, f"{tid} is defined twice (first at line {defs[tid][0]}); an ID is never reused")
                continue
            defs[tid], kinds[tid], text[tid] = (n + 1, col), kind, line
        if kind == "T":
            own = {tid for tid, _ in ids if tid[0] == "T"}
            named = sorted({m for m in MENTION.findall(line)})
            mentions.append((sorted(own), named, line))

    reds = {i for i, k in kinds.items() if k == "R"}
    greens = {i for i, k in kinds.items() if k == "T"}
    answered = set()

    for own, named, line in mentions:
        for r in named:
            if r not in reds:
                for t in own:
                    problem(path, defs[t][0], f"{t} names {r}, which does not exist (or is outside the paired region)")
        answered.update(r for r in named if r in reds)

    for t in sorted(greens):
        lineno, col = defs[t]
        line = text[t]
        if EXEMPT.search(line):
            if not REASON.search(line):
                problem(path, lineno, f"{t} carries TDD-Exempt:, which needs a reason")
            continue
        if OWN_RED.search(line):
            continue
        own = "R" + t[1:]
        named = [r for r in sorted(set(MENTION.findall(line))) if r in reds]
        candidates = ([own] if own in reds else []) + named
        if not candidates:
            problem(path, lineno, f"{t} has no red — add an R{t[1:]} above it, name the red it makes green "
                                  "(\"makes **R###** green\"), or say TDD-Exempt: <reason>")
            continue
        if own in reds:
            answered.add(own)
        for r in candidates:
            if defs[r] > (lineno, col):
                problem(path, lineno, f"{t} is listed before its red {r} (line {defs[r][0]}) — the test comes first")

    for r in sorted(reds - answered):
        problem(path, defs[r][0], f"{r} has no green — no T task is numbered like it, and none names it "
                                  "(a line that only REFERS to a red must not bold it: bold defines a task)")

    exempt = sum(1 for t in greens if EXEMPT.search(text[t]))
    own_red = sum(1 for t in greens if OWN_RED.search(text[t]) and not EXEMPT.search(text[t]))
    summary.append(f"{path}: {len(reds)} reds, {len(greens)} greens ({exempt} exempt, {own_red} their own red)")

if problems:
    print("\n".join(problems))
    sys.exit(1)
print("task pairing holds — every green has a red above it and every red has a green")
print("\n".join("    " + s for s in summary))
PY
