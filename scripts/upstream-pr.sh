#!/usr/bin/env bash
# upstream-pr.sh — open a clean PR into the upstream repo from a fork feature
# branch, converting the namespace back automatically.
#
# GitHub cannot rewrite a PR's diff, so the fork -> upstream namespace swap has
# to happen on the fork side, before the PR is opened. Given a feature branch
# (default: the current one) this:
#   1. fetches the upstream baseline (upstream/main)
#   2. starts a branch off it and lays down only the files the feature changed
#   3. converts them to the upstream namespace (scripts/namespace.sh to-upstream)
#   4. commits, builds, pushes to the fork, and opens the PR into upstream
#
# Fork-only files (README, LICENSE, badges, these scripts) live in the fork's
# main, not in the feature delta, so they never reach the PR. The feature is
# squashed into a single upstream commit; edit it on the PR if you want more.
#
# Requires on PATH: go, protoc (+ protoc-gen-go, protoc-gen-go-grpc), gofumpt,
# gh. gh must be authenticated with access to the upstream repo (this fork is a
# fork of it, so a cross-repo PR is allowed).
#
# Usage:
#   scripts/upstream-pr.sh [<feature-branch>] [-t "PR title"] [--dry-run]
set -euo pipefail

# Re-exec from a temp copy: step 2 checks out the upstream tree, which does not
# carry this fork-only script, so without this the running file would vanish
# mid-execution.
if [ "${_UPSTREAM_PR_REEXEC:-}" != "1" ]; then
  _self="$(mktemp)"; cat "$0" >"$_self"
  _UPSTREAM_PR_REEXEC=1 exec bash "$_self" "$@"
fi

UPSTREAM_REPO="datvietvac-techhub/open-streamer"
UPSTREAM_URL="https://github.com/${UPSTREAM_REPO}.git"
FORK_OWNER="ntt0601zcoder"
BASE_BRANCH="main"

title=""; feature=""; dry_run=0
while [ $# -gt 0 ]; do
  case "$1" in
    -t|--title)   title="${2:?--title needs a value}"; shift 2 ;;
    -n|--dry-run) dry_run=1; shift ;;
    -*)           echo "upstream-pr: unknown flag: $1" >&2; exit 2 ;;
    *)            feature="$1"; shift ;;
  esac
done

repo_root="$(git rev-parse --show-toplevel)"; cd "$repo_root"

if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
  echo "upstream-pr: tracked files have uncommitted changes — commit or stash first" >&2; exit 1
fi

feature="${feature:-$(git rev-parse --abbrev-ref HEAD)}"
if [ "$feature" = "$BASE_BRANCH" ]; then
  echo "upstream-pr: refusing to upstream '$BASE_BRANCH' — pass a feature branch" >&2; exit 2
fi
git rev-parse --verify --quiet "refs/heads/${feature}" >/dev/null \
  || { echo "upstream-pr: no such branch: ${feature}" >&2; exit 2; }

orig_branch="$(git rev-parse --abbrev-ref HEAD)"

# The transform tool lives on the fork only; the upstream-based work branch does
# not carry it, so stash a copy before switching.
ns_tool="$(mktemp)"; cp scripts/namespace.sh "$ns_tool"

# 1. upstream baseline
git remote get-url upstream >/dev/null 2>&1 || git remote add upstream "$UPSTREAM_URL"
git fetch --quiet upstream "$BASE_BRANCH"
git fetch --quiet origin "$BASE_BRANCH"

# 2. files the feature introduced relative to the fork's main
changed="$(git diff --name-only "origin/${BASE_BRANCH}...${feature}")"
if [ -z "$changed" ]; then
  echo "upstream-pr: ${feature} has no changes vs origin/${BASE_BRANCH}" >&2; exit 1
fi
n_files="$(printf '%s\n' "$changed" | grep -c .)"

work="upstream/${feature}"
git switch -C "$work" "upstream/${BASE_BRANCH}" >/dev/null 2>&1

# 3. lay down the feature's version of each changed file (handle deletions)
printf '%s\n' "$changed" | while IFS= read -r f; do
  [ -z "$f" ] && continue
  if git cat-file -e "${feature}:${f}" 2>/dev/null; then
    git checkout "$feature" -- "$f"
  else
    git rm --quiet --ignore-unmatch -- "$f" >/dev/null
  fi
done

# 4. convert the laid-down files to the upstream namespace + regen proto + gofumpt
bash "$ns_tool" to-upstream
rm -f "$ns_tool"

# 5. stage ONLY the feature's files (namespace.sh rewrote them in place) — never
#    `git add -A`, which would sweep in untracked junk or fork-only files that
#    happen to survive the branch switch.
printf '%s\n' "$changed" | while IFS= read -r f; do
  [ -n "$f" ] && git add -- "$f"
done
msg="${title:-$(git log -1 --format=%s "$feature")}"
git commit --quiet --no-verify -m "$msg"
echo "upstream-pr: ${work} = upstream/${BASE_BRANCH} + ${n_files} file(s) from ${feature} (upstream namespace)"
go build ./... >/dev/null
echo "upstream-pr: go build OK"

if [ "$dry_run" -eq 1 ]; then
  echo "upstream-pr: --dry-run — branch ready, NOT pushed. Diff vs upstream:"
  git --no-pager diff --stat "upstream/${BASE_BRANCH}" HEAD
  echo "upstream-pr: clean up with:  git switch ${orig_branch} && git branch -D ${work}"
  exit 0
fi

# 6. push to the fork + open the PR into upstream
git push --force --quiet origin "$work"
gh pr create --repo "$UPSTREAM_REPO" --base "$BASE_BRANCH" --head "${FORK_OWNER}:${work}" \
  --title "$msg" \
  --body "Ported from the fork. Namespace converted to upstream via \`scripts/namespace.sh\`; the feature is squashed into one commit."
git switch --quiet "$orig_branch"
echo "upstream-pr: PR opened into ${UPSTREAM_REPO}; back on ${orig_branch}"
