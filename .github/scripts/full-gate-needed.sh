#!/usr/bin/env bash
# Decides whether Check's full job runs the complete regression, and writes
# run=true or run=false to the step's outputs. A manual run always runs it. A
# merge queue entry runs it when the entry's own pull request has the full-gate
# label; its ref is gh-readonly-queue/BASE/pr-NUMBER-BASESHA. The label is read
# when the entry starts, so a label added later counts only once the entry is
# rebuilt.
#
# Each entry holds the pull requests ahead of it, but the queue only merges
# entries that pass their own required checks ("Only merge non-failing pull
# requests"), so each labelled pull request is gated in its own entry and the ones
# behind it keep the fast tier.
#
# full is a required check that succeeds when this says run=false, so anything
# unexpected exits with an error and fails the check instead.
set -euo pipefail

label=full-gate
case "${GITHUB_EVENT_NAME:-}" in
  workflow_dispatch)
    echo run=true >>"$GITHUB_OUTPUT"
    exit 0
    ;;
  merge_group) ;;
  *)
    echo "::error::full decides only for manual runs and merge queue entries, not '${GITHUB_EVENT_NAME:-}'"
    exit 1
    ;;
esac

if [[ ! "$GITHUB_REF" =~ ^(refs/heads/)?gh-readonly-queue/[^[:space:]]+/pr-([0-9]+)-[0-9a-f]{40,64}$ ]]; then
  echo "::error::'$GITHUB_REF' is not a merge queue entry"
  exit 1
fi
number=${BASH_REMATCH[2]}

labels=$(gh api "repos/$GITHUB_REPOSITORY/pulls/$number" --jq '.labels[].name')
if grep -Fqx "$label" <<<"$labels"; then
  echo "::notice::#$number has the $label label: running the complete regression on this entry"
  echo run=true >>"$GITHUB_OUTPUT"
else
  echo "#$number has no $label label: the fast check covers this entry"
  echo run=false >>"$GITHUB_OUTPUT"
fi
