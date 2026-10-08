#!/bin/sh
set -eu

lock_file="${RINSPACE_LATEX_PDF_LOCK_FILE:-/opt/rinspace/latex-pdf.lock.json}"
compiler="/usr/local/bin/rinspace-latex-compile"

fail() {
  printf '%s\n' "verify-latex-pdf-runtime: $*" >&2
  exit 1
}

[ -f "$lock_file" ] || fail "lock file is missing: $lock_file"
[ "$(id -u)" = 65532 ] || fail "runtime UID must be 65532"
[ "$(id -g)" = 65532 ] || fail "runtime GID must be 65532"
[ -x "$compiler" ] || fail "fixed compiler is missing"

json_scalar() {
  perl -MJSON::PP -e '
    my ($path, @keys) = @ARGV;
    open my $fh, "<", $path or die "cannot open lock: $!";
    local $/;
    my $value = decode_json(<$fh>);
    for my $key (@keys) { $value = $value->{$key}; }
    die "not a scalar" if ref($value);
    print $value;
  ' "$lock_file" "$@"
}

expected_compiler_sha="$(json_scalar compiler sha256)"
actual_compiler_sha="$(sha256sum "$compiler" | awk '{ print $1 }')"
[ "$actual_compiler_sha" = "$expected_compiler_sha" ] || fail "compiler digest mismatch"

json_packages="$(perl -MJSON::PP -e '
  open my $fh, "<", $ARGV[0] or die "cannot open lock: $!";
  local $/;
  my $packages = decode_json(<$fh>)->{packages};
  for my $name (sort keys %$packages) { print "$name=$packages->{$name}\n"; }
' "$lock_file")"
printf '%s\n' "$json_packages" | while IFS='=' read -r package expected; do
  [ -n "$package" ] || continue
  actual="$(dpkg-query -W -f='${Version}' "$package" 2>/dev/null || true)"
  [ "$actual" = "$expected" ] || fail "$package version mismatch: expected $expected, got ${actual:-missing}"
done

latexmk_version="$(latexmk -v 2>&1 | sed -n '1,3p' | tr '\n' ' ')"
case "$latexmk_version" in
  *"Version $(json_scalar compiler latexmkVersion)"*) ;;
  *) fail "latexmk version mismatch: $latexmk_version" ;;
esac

xelatex_version="$(xelatex --version | sed -n '1p')"
lualatex_version="$(lualatex --version | sed -n '1p')"
case "$xelatex_version" in *"TeX Live 2022/Debian"*) ;; *) fail "unexpected XeLaTeX version" ;; esac
case "$lualatex_version" in *"TeX Live 2022/Debian"*) ;; *) fail "unexpected LuaLaTeX version" ;; esac

[ "$(kpsewhich -var-value=openin_any)" = p ] || fail "openin_any is not paranoid"
[ "$(kpsewhich -var-value=openout_any)" = p ] || fail "openout_any is not paranoid"
[ "$(kpsewhich -var-value=shell_escape)" = f ] || fail "shell_escape is not disabled"
command -v tlmgr >/dev/null 2>&1 && fail "tlmgr must not be installed"

for font_package in fonts-lmodern fonts-noto-cjk fonts-texgyre; do
  dpkg-query -W "$font_package" >/dev/null 2>&1 || fail "font package is missing: $font_package"
done

printf 'policyId=%s\n' "$(json_scalar policyId)"
printf 'compilerSha256=%s\n' "$actual_compiler_sha"
printf 'latexmk=%s\n' "$latexmk_version"
printf 'xelatex=%s\n' "$xelatex_version"
printf 'lualatex=%s\n' "$lualatex_version"
printf '%s\n' "$json_packages"
printf '%s\n' 'latex-pdf runtime verification passed'
