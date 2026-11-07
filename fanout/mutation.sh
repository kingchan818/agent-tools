#!/usr/bin/env bash
# Mutation testing: every gremlins mutator, every package. Prints each
# package's score and the mutants that survived (LIVED) or were not covered.
set -euo pipefail
cd "$(dirname "$0")"
GREMLINS="${GREMLINS:-$(go env GOPATH)/bin/gremlins}"
pkgs=("${@:-./internal/agentdef ./internal/cli ./internal/hc ./internal/orch ./internal/store ./internal/task ./internal/thread}")
status=0
for pkg in ${pkgs[@]}; do
  echo "== $pkg"
  out=$("$GREMLINS" unleash --workers "${WORKERS:-1}" --timeout-coefficient 30 \
    --invert-assignments --invert-bitwise --invert-bwassign --invert-logical \
    --invert-loopctrl --remove-self-assignments "$pkg" 2>&1) || true
  # Here-strings, not pipes: grep -q exits early and pipefail would turn
  # the writer's SIGPIPE into a false "no match".
  grep -E '^\s+(LIVED|NOT COVERED|TIMED OUT)' <<<"$out" || true
  grep -E '^(Killed|Timed out|Test efficacy|Mutator coverage)' <<<"$out" || true
  if grep -qE '^\s+(LIVED|NOT COVERED)' <<<"$out"; then status=1; fi
done
exit $status
