#!/usr/bin/env bash
# End-to-end check against real hc and omp: two parallel workers, a
# background run with a follow-up, and a worktree edit. Uses a scratch state
# dir and repo; leaves no terminals behind.
set -euo pipefail
cd "$(dirname "$0")"
go build -o fanout ./cmd/fanout
BIN="$PWD/fanout"
work=$(mktemp -d)
export FANOUT_HOME="$work/state"
trap '"$BIN" kill last >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
cd "$work"
printf 'package calc\n\nfunc Add(a, b int) int { return a - b }\n' > calc.go
git init -q && git add . && git -c user.email=e2e@x -c user.name=e2e commit -qm init

echo "== parallel run"
"$BIN" run --timeout 5m \
  "Read calc.go and say whether Add is correct. Do not edit files." \
  "What is 17*23? Answer with the number." >out.txt
grep -q "2 done" out.txt && grep -q "391" out.txt
echo "ok"

echo "== background run, wait, follow-up"
"$BIN" run --bg --keep "Write a report that says exactly: ALPHA" >/dev/null
"$BIN" wait --timeout 5m | grep -q ALPHA
"$BIN" send last 1 "Rewrite the report so it says exactly: BETA" --timeout 5m | grep -q BETA
"$BIN" kill last >/dev/null
echo "ok"

echo "== worktree edit"
"$BIN" run --worktree --timeout 5m "Fix Add in calc.go so it adds. Commit the fix on your branch." >/dev/null
commits=$(git log --all --oneline)
[ "$(printf '%s\n' "$commits" | wc -l)" -ge 2 ] || { echo "no fix commit"; echo "$commits"; exit 1; }
echo "ok"

left=$(hc ls --json | grep -c '"fo-' || true)
[ "$left" -eq 0 ] || { echo "leftover terminals"; exit 1; }
echo "e2e passed"
