#!/usr/bin/env bash
#
# Verifies an extracted, immutable Typst HTML toolchain before the Renderer API
# runtime env is allowed to point at it. The same script is executed by the
# self-hosted toolchain workflow against the artifact it just assembled, so a
# promoted commit-addressed directory is checked by the exact same contract.
#
# usage: verify-typst-html-toolchain.sh --root <artifact-root> [--lock <lock.json>]
set -euo pipefail

fail() {
  printf 'verify-typst-html-toolchain: %s\n' "$*" >&2
  exit 1
}

root=""
lock=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --root)
      [ "$#" -ge 2 ] || fail "--root requires a value"
      root="$2"
      shift 2
      ;;
    --lock)
      [ "$#" -ge 2 ] || fail "--lock requires a value"
      lock="$2"
      shift 2
      ;;
    -h|--help)
      printf '%s\n' 'usage: verify-typst-html-toolchain.sh --root <artifact-root> [--lock <lock.json>]'
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[ -n "$root" ] || fail "--root is required"
[ -d "$root" ] || fail "artifact root is missing: $root"

script_directory="$(cd "$(dirname "$0")" && pwd)"
if [ -z "$lock" ]; then
  for candidate in "$root/typst-html-toolchain.lock.json" "${script_directory}/typst-html-toolchain.lock.json"; do
    if [ -f "$candidate" ]; then
      lock="$candidate"
      break
    fi
  done
fi
[ -n "$lock" ] || fail "lock file is missing: pass --lock"
[ -f "$lock" ] || fail "lock file is missing: $lock"
command -v node >/dev/null 2>&1 || fail "node is required to read the lock file"

# The lock is the single source of truth. Every pin below is re-derived from it
# instead of being duplicated in this script.
pins="$(node --input-type=module -e '
  import crypto from "node:crypto";
  import fs from "node:fs";
  const lock = JSON.parse(fs.readFileSync(process.argv[1], "utf8"));
  if (lock.schemaVersion !== "rin-typst-html-toolchain-lock/v1") throw new Error("unexpected lock schema");
  const compiler = lock.compiler;
  const font = lock.font;
  const packages = lock.packages;
  const profile = lock.profile;
  const expected = crypto.createHash("sha256").update([...packages.approved].sort().join("\n")).digest("hex");
  if (expected !== packages.packageSetDigest) throw new Error("package set digest in the lock is not reproducible");
  const rows = {
    version: compiler.version,
    binaryRelativePath: compiler.binaryRelativePath,
    binarySha256: compiler.binarySha256,
    fontRelativePath: font.relativePath,
    fontSha256: font.sha256,
    packagesRelativePath: packages.relativePath,
    archiveRoot: lock.artifact.archiveRoot,
    profileId: profile.id,
  };
  for (const [key, value] of Object.entries(rows)) {
    if (typeof value !== "string" || value.length === 0) throw new Error(`lock field ${key} is missing`);
    if (/[\n\r=]/.test(value)) throw new Error(`lock field ${key} is not a flat scalar`);
    console.log(`${key}=${value}`);
  }
' "$lock")" || fail "the lock file is not usable"

while IFS='=' read -r key value; do
  case "$key" in
    version) version="$value" ;;
    binaryRelativePath) binary_relative="$value" ;;
    binarySha256) binary_sha="$value" ;;
    fontRelativePath) font_relative="$value" ;;
    fontSha256) font_sha="$value" ;;
    packagesRelativePath) packages_relative="$value" ;;
    archiveRoot) archive_root="$value" ;;
    profileId) profile_id="$value" ;;
  esac
done <<<"$pins"

for required in version binary_relative binary_sha font_relative font_sha packages_relative archive_root profile_id; do
  eval "value=\${$required:-}"
  [ -n "$value" ] || fail "the lock file is incomplete: $required"
done

for digest in "$binary_sha" "$font_sha"; do
  case "$digest" in
    [0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ;;
    *) fail "the lock file carries a malformed sha256: $digest" ;;
  esac
done

archive_root="${archive_root%/}"
binary="${root}/${archive_root}/${binary_relative}"
font="${root}/${archive_root}/${font_relative}"
package_path="${root}/${archive_root}/${packages_relative}"

[ -f "$binary" ] || fail "pinned Typst binary is missing: $binary"
[ -x "$binary" ] || fail "pinned Typst binary is not executable: $binary"
[ -f "$font" ] || fail "pinned font is missing: $font"
[ -d "$package_path" ] || fail "pinned package path is missing: $package_path"

binary_digest="$(sha256sum "$binary" | awk '{ print $1 }')"
font_digest="$(sha256sum "$font" | awk '{ print $1 }')"
[ "$binary_digest" = "$binary_sha" ] || fail "Typst binary digest mismatch: $binary_digest"
[ "$font_digest" = "$font_sha" ] || fail "pinned font digest mismatch: $font_digest"

binary_version="$("$binary" --version | sed -n '1p')"
case "$binary_version" in
  "typst ${version}"*) ;;
  *) fail "Typst version mismatch: $binary_version" ;;
esac

# Package downloads must stay impossible at runtime: the compiler is forced to
# use an empty, non-writable package root that never carries vendored packages.
[ -z "$(ls -A "$package_path" 2>/dev/null || true)" ] || fail "Typst package path must stay empty"
package_mode="$(stat -c '%a' "$package_path")"
# Permission bits, not -w: the check must hold for any installer UID, including
# one that can bypass the write bit.
[ "$(( 8#${package_mode} & 0222 ))" -eq 0 ] || fail "Typst package path must not be writable (mode ${package_mode})"

printf 'profileId=%s\n' "$profile_id"
printf 'typst=%s\n' "$binary_version"
printf 'binarySha256=%s\n' "$binary_digest"
printf 'fontSha256=%s\n' "$font_digest"
printf '%s\n' 'typst-html toolchain verification passed'
