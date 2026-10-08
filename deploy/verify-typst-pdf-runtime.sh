#!/bin/sh
set -eu

lock_file="${RINSPACE_TYPST_PDF_LOCK_FILE:-/opt/rinspace/typst-pdf.lock.json}"
pins_file="${RINSPACE_TYPST_PDF_PINS_FILE:-/opt/rinspace/typst-pdf.pins}"

fail() {
  printf '%s\n' "verify-typst-pdf-runtime: $*" >&2
  exit 1
}

# The runtime image has no JSON tooling by design: the Dockerfile pins the same
# scalars into a flat file whose values typst-pdf-policy.test.mjs proves equal to
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
policy="$(pin policyId)"

for value in "$version" "$binary" "$binary_sha" "$compiler" "$compiler_sha" "$font" "$font_sha" "$package_path" "$package_cache_path" "$policy"; do
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

# Package downloads must stay impossible: no network tooling in the image and an
# empty package root that the compiler is forced to use.
[ -z "$(ls -A "$package_path" 2>/dev/null)" ] || fail "Typst package path must stay empty"
[ -z "$(ls -A "$package_cache_path" 2>/dev/null)" ] || fail "Typst package cache must stay empty"
[ -w "$package_cache_path" ] && fail "Typst package cache must not be writable"
for forbidden in curl wget git gcc cc tex latex; do
  command -v "$forbidden" >/dev/null 2>&1 && fail "$forbidden must not be installed"
done

printf 'policyId=%s\n' "$policy"
printf 'typst=%s\n' "$binary_version"
printf 'binarySha256=%s\n' "$binary_sha"
printf 'compilerSha256=%s\n' "$compiler_sha"
printf 'fontSha256=%s\n' "$font_sha"
printf '%s\n' 'typst-pdf runtime verification passed'
