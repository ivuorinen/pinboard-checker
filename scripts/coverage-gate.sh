#!/usr/bin/env bash
# Fail unless statement coverage reaches the threshold, ignoring blocks the
# source marks as unreachable.
#
# The suite is at 100%, and a bare number in CI output does not keep it there --
# nothing failed when coverage dropped, so a drop was only ever noticed by
# someone reading the log. This gate is what makes the 100% a contract.
#
# Go has no coverage pragma, so the escape hatch is a comment convention:
#
#     if err != nil {
#         // coverage:ignore <reason>
#         return ...
#     }
#
# The marker goes *inside* the unreachable block, above the statement it
# excuses; the reason may run over several comment lines. It binds to the next
# line that is neither blank nor a comment, which is the line Go's profile
# reports, and excludes exactly that one block.
#
# Use it only where a branch cannot execute -- `json.Marshal` on a
# map[string]string, say -- never to hide a branch that is merely inconvenient
# to reach. Every use states why, and a reviewer should treat a new one as a
# claim to check rather than a formality.
#
# Usage: coverage-gate.sh [profile] [threshold-percent]
set -euo pipefail

cd "$(dirname "$0")/.."

profile=${1:-coverage.out}
threshold=${2:-100}

[ -f "$profile" ] || {
  printf 'coverage-gate: %s not found; run the tests with -coverprofile first\n' \
    "$profile" >&2
  exit 1
}

# file:line for every ignore marker, resolved to the line the profile will
# actually report. A marker sits above the code it excuses, usually with the
# rest of its reason on the comment lines between, so it binds to the next line
# that is neither blank nor a comment -- the statement Go counts.
markers=$(mktemp)
trap 'rm -f "$markers"' EXIT

grep -rln 'coverage:ignore' --include='*.go' . 2>/dev/null |
  while IFS= read -r file; do
    awk -v file="${file#./}" '
      /coverage:ignore/ { pending = 1; print file ":" NR; next }
      pending && !/^[[:space:]]*(\/\/.*)?$/ { print file ":" NR; pending = 0 }
    ' "$file"
  done >"$markers"

awk -v markers="$markers" -v threshold="$threshold" '
BEGIN {
  while ((getline m < markers) > 0) ignore[m] = 1
}
NR == 1 { next }                       # mode: line
{
  # <import/path>/<file>.go:<startLine>.<col>,<endLine>.<col> <stmts> <count>
  split($1, location, ":")
  path = location[1]
  sub(/.*\//, "", path)                # profile paths are import paths
  split(location[2], span, ",")
  split(span[1], start, ".")

  stmts = $2
  count = $3

  if (count == 0 && (path ":" start[1]) in ignore) {
    ignored += stmts
    ignoredBlocks++
    next
  }

  total += stmts
  if (count > 0) covered += stmts
  else printf "  uncovered: %s:%s (%d statement(s))\n", path, start[1], stmts
}
END {
  if (total == 0) { print "coverage-gate: no statements found"; exit 1 }

  percent = covered * 100 / total
  printf "coverage-gate: %.1f%% (%d/%d statements", percent, covered, total
  if (ignoredBlocks) printf ", %d ignored in %d block(s)", ignored, ignoredBlocks
  printf ")\n"

  if (percent + 0.0001 < threshold) {
    printf "coverage-gate: FAIL - below the %s%% threshold\n", threshold
    exit 1
  }
  print "coverage-gate: OK"
}
' "$profile"
