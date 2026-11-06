#!/bin/sh
# Build the Linux x64 SpawnPoint executable. No Git tags or cgo required.
set -eu

if [ "$#" -gt 1 ]; then
  echo "Usage: sh tools/dist.sh [output-directory]" >&2
  exit 2
fi

script_dir=$(CDPATH= cd "$(dirname "$0")" && pwd -P)
repo_root=$(CDPATH= cd "$script_dir/.." && pwd -P)
out_dir="dist"
if [ "$#" -eq 1 ]; then out_dir="$1"; fi
case "$out_dir" in
  /*) output_root="$out_dir" ;;
  *) output_root="$repo_root/$out_dir" ;;
esac

if ! command -v go >/dev/null 2>&1; then
  echo "Go 1.26 or newer must be installed and on PATH." >&2
  exit 1
fi

target_dir="$output_root/linux64"
mkdir -p "$target_dir"
exe="$target_dir/spawnpoint"
printf 'Building SpawnPoint for Linux x64...\n'
(
  cd "$repo_root"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -buildvcs=false -o "$exe" ./cmd/spawnpoint
)
if [ ! -x "$exe" ]; then
  echo "Build completed without an executable at $exe" >&2
  exit 1
fi

printf 'Built: %s\n' "$exe"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$exe"
elif command -v shasum >/dev/null 2>&1; then
  shasum -a 256 "$exe"
fi
