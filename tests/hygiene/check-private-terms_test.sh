#!/usr/bin/env bash
# Tests scripts/check-private-terms.sh in a throwaway repository, with a
# made-up term.
set -uo pipefail
script="$(cd "$(dirname "$0")/../.." && pwd)/scripts/check-private-terms.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cd "$work"
git init -q . && git config user.email t@example.com && git config user.name t
echo "hello" >a.txt && git add a.txt && git commit -q -m "first"
base="$(git rev-parse HEAD)"
export FORBIDDEN_TERMS="zebra-corp, Quokka"
failures=0
expect() { # expect <status> <name> <command...>
  want="$1"; name="$2"; shift 2
  out="$("$@" 2>&1)"; got=$?
  if [ "$got" -ne "$want" ]; then echo "FAIL $name: exit $got, want $want"; echo "$out"; failures=$((failures + 1)); fi
  if printf '%s' "$out" | grep -q -i -E 'zebra-corp|quokka'; then echo "FAIL $name: the output shows a term"; failures=$((failures + 1)); fi
}

expect 0 "a clean tree passes" "$script" "$base"
FORBIDDEN_TERMS="" expect 0 "unset terms skip the check" "$script" "$base"

printf 'x\n' >bin.dat && printf 'ZEBRA-CORP\0' >>bin.dat && git add bin.dat && git commit -q -m "binary"
expect 0 "binary files are skipped" "$script" "$base"

echo "uses Zebra-Corp internally" >b.md && git add b.md && git commit -q -m "docs"
expect 1 "a term in a file fails, case-insensitively" "$script" "$base"
git rm -q b.md && git commit -q -m "remove"
expect 0 "a removed file passes" "$script" "$base"

git commit -q --allow-empty -m "for the QUOKKA project"
expect 1 "a term in a commit message fails" "$script" "$base"
expect 0 "a term in an older commit message is ignored" "$script" "$(git rev-parse HEAD)"

PR_TEXT="Add support for zebra-corp" expect 1 "a term in the PR text fails" "$script" "$(git rev-parse HEAD)"
PR_TEXT="An ordinary change" expect 0 "ordinary PR text passes" "$script" "$(git rev-parse HEAD)"

if [ "$failures" -ne 0 ]; then echo "$failures failure(s)"; exit 1; fi
echo "ok"
