#!/usr/bin/env bash
# namespace.sh — swap the GitHub namespace of this module between the fork
# and upstream.
#
# The fork is developed in the `ntt0601zcoder` namespace; the upstream
# `datvietvac-techhub` tree is produced on demand when contributing back.
# GitHub cannot rewrite a PR's diff, so the conversion happens HERE, on the
# fork side, before the PR is opened — base it on upstream/main and the
# namespace lines match, so the PR shows only the real change.
#
#   scripts/namespace.sh to-fork      # datvietvac-techhub -> ntt0601zcoder
#   scripts/namespace.sh to-upstream  # ntt0601zcoder      -> datvietvac-techhub
#
# What it rewrites: the module path in go.mod, every Go import, the
# golangci module-path, and the proto go_package option. The generated
# *.pb.go files are NOT edited in place — their rawDesc embeds a
# length-prefixed copy of go_package, so a byte-level edit corrupts the
# descriptor and panics at init. They are regenerated from the rewritten
# .proto instead.
#
# Build entry points (Makefile, release workflow, dev script, Dockerfile)
# derive the package path from `go list -m`, so they follow the rename
# automatically and need no rewrite here.
#
# Requires: protoc + protoc-gen-go + protoc-gen-go-grpc on PATH, matching
# the versions stamped in the generated files.
set -euo pipefail

case "${1:-}" in
  to-fork)     FROM=datvietvac-techhub; TO=ntt0601zcoder ;;
  to-upstream) FROM=ntt0601zcoder;      TO=datvietvac-techhub ;;
  *) echo "usage: $(basename "$0") {to-fork|to-upstream}" >&2; exit 2 ;;
esac

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

old="github.com/${FROM}/open-streamer"
new="github.com/${TO}/open-streamer"

# 1. Rewrite the namespace token in every hand-written file. Generated
#    protobuf is excluded here and regenerated in step 2. `sed -i.bak` is
#    portable across BSD (macOS) and GNU sed; the backup is removed after.
files="$(grep -rl "$old" \
  --include='*.go' --include='go.mod' --include='*.yml' --include='*.proto' \
  . --exclude-dir=.git | grep -v '\.pb\.go$' || true)"

if [ -z "$files" ]; then
  echo "namespace: source already at github.com/${TO}/open-streamer"
else
  count=0
  for f in $files; do
    sed -i.bak "s|${old}|${new}|g" "$f" && rm -f "$f.bak"
    count=$((count + 1))
  done
  echo "namespace: rewrote ${count} file(s) ${FROM} -> ${TO}"
fi

# 2. Regenerate protobuf from the rewritten go_package so the rawDesc
#    length prefixes stay consistent with the new path.
protoc -I internal/transcoder/native/proto \
  --go_out=internal/transcoder/native/proto --go_opt=paths=source_relative \
  --go-grpc_out=internal/transcoder/native/proto --go-grpc_opt=paths=source_relative \
  internal/transcoder/native/proto/transcoder.proto

echo "namespace: regenerated protobuf -> github.com/${TO}/open-streamer"

# 3. Re-sort imports. Shortening/lengthening the module token moves the
#    internal imports relative to third-party ones, so the import blocks
#    need re-grouping or gofumpt (the lint formatter) fails.
if command -v gofumpt >/dev/null 2>&1; then
  gofumpt -w .
  echo "namespace: gofumpt -w (re-sorted imports)"
else
  gofmt -w .
  echo "namespace: gofmt -w (install gofumpt for the lint-exact format)"
fi
