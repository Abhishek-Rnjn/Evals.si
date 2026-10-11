#!/usr/bin/env bash
# Fails if a private term (a company, cluster or customer name that must not
# be published) appears in a tracked text file, in a commit message of the
# range BASE..HEAD, or in the pull request title and description.
#
# The terms come from the FORBIDDEN_TERMS environment variable (comma or
# newline separated), which CI takes from a repository secret. The list is not
# kept in the repository: a file listing the words would publish them. Matching
# is case-insensitive and literal. Output names files, line numbers and commit
# ids only, never the matching text.
#
#   FORBIDDEN_TERMS=... [PR_TEXT=...] scripts/check-private-terms.sh [BASE]
#
# With FORBIDDEN_TERMS unset (for example on a pull request from a fork, where
# secrets are not available) the check is skipped with a warning.
set -euo pipefail

base="${1:-}"
if [ -z "${FORBIDDEN_TERMS:-}" ]; then
  echo "::warning::FORBIDDEN_TERMS is not set, so the private-term check did not run"
  exit 0
fi

terms="$(mktemp)"
trap 'rm -f "$terms"' EXIT
printf '%s\n' "$FORBIDDEN_TERMS" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | grep -v '^$' >"$terms" || true
if [ ! -s "$terms" ]; then
  echo "::warning::FORBIDDEN_TERMS has no terms, so the private-term check did not run"
  exit 0
fi

found=0

# Tracked text files in the current tree (binary files are skipped).
if hits="$(git grep -I -i -n -F -f "$terms" -- . | cut -d: -f1,2)" && [ -n "$hits" ]; then
  echo "A private term is in these files (file:line):"
  echo "$hits"
  found=1
fi

# Commit messages of the new commits.
if [ -n "$base" ] && [ "$base" != 0000000000000000000000000000000000000000 ] && git rev-parse -q --verify "$base^{commit}" >/dev/null; then
  for commit in $(git rev-list "$base..HEAD"); do
    if git log -1 --format=%B "$commit" | grep -q -i -F -f "$terms"; then
      echo "A private term is in the message of commit $commit"
      found=1
    fi
  done
fi

# The pull request title and description.
if [ -n "${PR_TEXT:-}" ] && printf '%s' "$PR_TEXT" | grep -q -i -F -f "$terms"; then
  echo "A private term is in the pull request title or description"
  found=1
fi

if [ "$found" -ne 0 ]; then
  echo "Remove it (and see CONTRIBUTING.md on keeping private names out of the repository)."
  exit 1
fi
echo "No private terms found."
