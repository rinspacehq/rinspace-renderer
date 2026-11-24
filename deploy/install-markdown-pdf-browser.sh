#!/usr/bin/env bash
set -euo pipefail

lock="$(realpath "${1:?usage: install-markdown-pdf-browser.sh <lock> <output-directory> [archive]}")"
output="${2:?output directory required}"
scratch="$(mktemp -d)"
trap 'rm -rf -- "$scratch"' EXIT

locked() {
  node -p 'require(process.argv[1])[process.argv[2]]' "$lock" "$1"
}

test "$(locked platform)" = linux/amd64
archive="${3:-$scratch/browser.zip}"
if [[ $# -eq 2 ]]; then
  curl --fail --silent --show-error --location --proto '=https' --retry 2 --max-time 120 \
    "$(locked release)" -o "$archive"
fi
printf '%s  %s\n' "$(locked archiveSha256)" "$archive" | sha256sum -c -
unzip -q "$archive" -d "$scratch"
binary="$scratch/$(locked executable)"
printf '%s  %s\n' "$(locked executableSha256)" "$binary" | sha256sum -c -
test -x "$binary"
test -s "$scratch/$(locked notice)"
mkdir -p "$output"
cp -a "$(dirname "$binary")/." "$output/"
