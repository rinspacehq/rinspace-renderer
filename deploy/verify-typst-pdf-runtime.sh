#!/bin/sh
set -eu

lock_file="${RINSPACE_TYPST_PDF_LOCK_FILE:-/opt/rinspace/typst-pdf.lock.json}"
pins_file="${RINSPACE_TYPST_PDF_PINS_FILE:-/opt/rinspace/typst-pdf.pins}"

fail() {
  printf '%s\n' "verify-typst-pdf-runtime: $*" >&2
  exit 1
}

# The runtime image has no JSON tooling by design: the Dockerfile pins the same
# scalars into a flat file whose values source-policy.test.mjs proves equal to
# typst-pdf.lock.json.
[ -f "$lock_file" ] || fail "lock file is missing: $lock_file"
[ -f "$pins_file" ] || fail "pin file is missing: $pins_file"

pin() {
  sed -n "s/^$1=//p" "$pins_file" | head -n 1
}

version="$(pin version)"
binary="$(pin binaryPath)"
binary_sha="$(pin binarySha256)"
compiler="$(pin compilerPath)"
compiler_sha="$(pin compilerSha256)"
font="$(pin fontPath)"
font_sha="$(pin fontSha256)"
package_path="$(pin packagePath)"
package_cache_path="$(pin packageCachePath)"
package_sha="$(pin packageSetSha256)"
policy="$(pin policyId)"

for value in "$version" "$binary" "$binary_sha" "$compiler" "$compiler_sha" "$font" "$font_sha" "$package_path" "$package_cache_path" "$package_sha" "$policy"; do
  [ -n "$value" ] || fail "pin file is incomplete"
done

[ "$(id -u)" = 65532 ] || fail "runtime UID must be 65532"
[ "$(id -g)" = 65532 ] || fail "runtime GID must be 65532"
[ -x "$compiler" ] || fail "fixed compiler is missing"
[ -x "$binary" ] || fail "pinned Typst binary is missing"
[ -f "$font" ] || fail "pinned font is missing"
[ -d "$package_path" ] || fail "pinned package path is missing"
[ -d "$package_cache_path" ] || fail "pinned package cache path is missing"

[ "$(sha256sum "$compiler" | awk '{ print $1 }')" = "$compiler_sha" ] || fail "compiler digest mismatch"
[ "$(sha256sum "$binary" | awk '{ print $1 }')" = "$binary_sha" ] || fail "Typst binary digest mismatch"
[ "$(sha256sum "$font" | awk '{ print $1 }')" = "$font_sha" ] || fail "pinned font digest mismatch"

binary_version="$("$binary" --version | sed -n '1p')"
case "$binary_version" in
  "typst $version"*) ;;
  *) fail "Typst version mismatch: $binary_version" ;;
esac

# Only the build-time approved preview packages are present. The cache and
# local package directory are read-only and the worker runs without network.
[ -z "$(ls -A "$package_path" 2>/dev/null)" ] || fail "Typst local package path must stay empty"
[ -z "$(find "$package_cache_path" ! -type f ! -type d -print -quit)" ] || fail "Typst package cache contains a non-regular entry"
[ -z "$(find "$package_cache_path" -writable -print -quit)" ] || fail "Typst package cache must not be writable"
package_digest="$(
  cd "$package_cache_path"
  LC_ALL=C find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do
    digest="$(sha256sum -- "$file")"
    bytes="$(stat -c '%s' -- "$file")"
    printf '%s\000%s\000%s\n' "${file#./}" "${digest%% *}" "$bytes"
  done | sha256sum | awk '{ print $1 }'
)"
[ "$package_digest" = "$package_sha" ] || fail "sealed Typst package cache digest mismatch"
for forbidden in curl wget git gcc cc tex latex; do
  command -v "$forbidden" >/dev/null 2>&1 && fail "$forbidden must not be installed"
done

printf 'policyId=%s\n' "$policy"
printf 'typst=%s\n' "$binary_version"
printf 'binarySha256=%s\n' "$binary_sha"
printf 'compilerSha256=%s\n' "$compiler_sha"
printf 'fontSha256=%s\n' "$font_sha"
printf 'packageSetSha256=%s\n' "$package_sha"
printf '%s\n' 'typst-pdf runtime verification passed'
