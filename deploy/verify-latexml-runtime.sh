#!/usr/bin/env sh
set -eu

LOCK_FILE="${RIN_RENDERER_LATEXML_LOCK_FILE:-/opt/rin-renderer/engines/latexml/latexml.lock}"
LATEXML_SOURCE_DIR="${RIN_RENDERER_LATEXML_SOURCE_DIR:-/opt/LaTeXML}"
LATEXMLC_BIN="${RIN_RENDERER_LATEXML_BIN:-latexmlc}"
XELATEX_BIN="${RIN_RENDERER_XELATEX_BIN:-xelatex}"
DVISVGM_BIN="${RIN_RENDERER_DVISVGM_BIN:-dvisvgm}"
TEXSVG_BIN="${RIN_RENDERER_TEXSVG_BIN:-rin-renderer-texsvg}"
MATHJAX_NODE_BIN="${RIN_RENDERER_MATHJAX_NODE_BIN:-node}"
MATHJAX_SCRIPT="${RIN_RENDERER_MATHJAX_SCRIPT:-/opt/rin-renderer/engines/mathjax/render-mathjax.mjs}"
KATEX_NODE_BIN="${RIN_RENDERER_KATEX_NODE_BIN:-node}"
KATEX_SCRIPT="${RIN_RENDERER_KATEX_SCRIPT:-/opt/rin-renderer/engines/katex/render-katex.mjs}"

fail() {
  printf '%s\n' "verify-latexml-runtime: $*" >&2
  exit 1
}

json_field() {
  perl -MJSON::PP -e '
    my ($field, $path) = @ARGV;
    open my $fh, "<", $path or die "cannot open $path: $!";
    local $/;
    my $json = decode_json(<$fh>);
    my $value = $json->{$field};
    print defined($value) ? $value : "";
  ' "$1" "$LOCK_FILE"
}

required_json_field() {
  value="$(json_field "$1")"
  if [ -z "$value" ]; then
    fail "latexml.lock missing required field: $1"
  fi
  printf '%s' "$value"
}

command_exists() {
  command -v "$1" >/dev/null 2>&1
}

require_command() {
  label="$1"
  bin="$2"
  if ! command_exists "$bin"; then
    fail "$label is not available: $bin"
  fi
  printf '%s: %s\n' "$label" "$(command -v "$bin")"
}

if [ ! -f "$LOCK_FILE" ]; then
  fail "latexml.lock not found: $LOCK_FILE"
fi

expected_repo="$(required_json_field repo)"
expected_commit="$(required_json_field commit)"
expected_texlive="$(required_json_field texlive)"
expected_perl="$(required_json_field perl)"
expected_adapter="$(required_json_field adapterVersion)"
expected_tag="$(json_field tag)"

printf '%s\n' "LaTeXML lock:"
printf '  repo: %s\n' "$expected_repo"
printf '  commit: %s\n' "$expected_commit"
if [ -n "$expected_tag" ]; then
  printf '  tag: %s\n' "$expected_tag"
fi
printf '  texlive: %s\n' "$expected_texlive"
printf '  perl: %s\n' "$expected_perl"
printf '  adapterVersion: %s\n' "$expected_adapter"

runtime_perl="$(perl -e 'print substr($^V, 1)')"
case "$runtime_perl" in
  "$expected_perl"|"$expected_perl".*) ;;
  *) fail "Perl runtime mismatch: expected $expected_perl, got $runtime_perl" ;;
esac
printf 'Runtime Perl: %s\n' "$runtime_perl"

if ! command_exists pdftex; then
  fail "pdftex is not available; TeXLive runtime cannot be verified"
fi
runtime_texlive="$(pdftex --version | sed -n '1p')"
case "$runtime_texlive" in
  *"$expected_texlive"*) ;;
  *) fail "TeXLive runtime mismatch: expected line containing '$expected_texlive', got '$runtime_texlive'" ;;
esac
printf 'Runtime TeXLive: %s\n' "$runtime_texlive"

require_command "XeLaTeX" "$XELATEX_BIN"
runtime_xelatex="$("$XELATEX_BIN" --version 2>&1 | sed -n '1p')"
printf 'Runtime XeLaTeX: %s\n' "$runtime_xelatex"

require_command "dvisvgm" "$DVISVGM_BIN"
runtime_dvisvgm="$("$DVISVGM_BIN" --version 2>&1 | sed -n '1p')"
printf 'Runtime dvisvgm: %s\n' "$runtime_dvisvgm"

require_command "Rin TeX SVG worker" "$TEXSVG_BIN"
require_command "Node.js for MathJax" "$MATHJAX_NODE_BIN"
if [ ! -f "$MATHJAX_SCRIPT" ]; then
  fail "MathJax renderer script is not available: $MATHJAX_SCRIPT"
fi
mathjax_smoke="$(printf '%s\n' '{"source":"\\not\\exists x","displayMode":true}' | "$MATHJAX_NODE_BIN" "$MATHJAX_SCRIPT")"
case "$mathjax_smoke" in
  *'"ok":true'*'"engine":"mathjax-chtml"'*) ;;
  *) fail "MathJax renderer smoke failed: $mathjax_smoke" ;;
esac
printf 'Runtime MathJax: %s\n' "$(printf '%s' "$mathjax_smoke" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"
mathjax_warm_smoke="$(printf '%s\n' '{"contractVersion":"rin-node-worker/v1","id":"runtime-smoke","operation":"mathjax.render","payload":{"source":"\\not\\exists x","displayMode":true}}' | "$MATHJAX_NODE_BIN" "$MATHJAX_SCRIPT" --ndjson-worker)"
case "$mathjax_warm_smoke" in
  *'"contractVersion":"rin-node-worker/v1"'*'"id":"runtime-smoke"'*'"ok":true'*'"engine":"mathjax-chtml"'*'"rssBytes":'*) ;;
  *) fail "Warm MathJax NDJSON smoke failed: $mathjax_warm_smoke" ;;
esac
printf 'Runtime MathJax warm worker: rin-node-worker/v1 NDJSON stdio\n'

require_command "Node.js" "$KATEX_NODE_BIN"
if [ ! -f "$KATEX_SCRIPT" ]; then
  fail "KaTeX renderer script is not available: $KATEX_SCRIPT"
fi
katex_smoke="$(printf '%s\n' '{"source":"\\not\\exists x","displayMode":true}' | "$KATEX_NODE_BIN" "$KATEX_SCRIPT")"
case "$katex_smoke" in
  *'"ok":true'*'"engine":"katex"'*) ;;
  *) fail "KaTeX renderer smoke failed: $katex_smoke" ;;
esac
printf 'Runtime KaTeX: %s\n' "$(printf '%s' "$katex_smoke" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p')"

if ! command_exists "$LATEXMLC_BIN"; then
  fail "latexmlc is not available: $LATEXMLC_BIN"
fi
runtime_latexml="$("$LATEXMLC_BIN" --VERSION 2>&1 | sed -n '1p')"
printf 'Runtime LaTeXML: %s\n' "$runtime_latexml"

runtime_commit="${RIN_RENDERER_LATEXML_VERSION:-}"
if [ -d "$LATEXML_SOURCE_DIR/.git" ] && command_exists git; then
  runtime_commit="$(git -C "$LATEXML_SOURCE_DIR" rev-parse HEAD)"
fi
if [ -z "$runtime_commit" ]; then
  fail "LaTeXML runtime commit is unavailable; set RIN_RENDERER_LATEXML_VERSION or keep $LATEXML_SOURCE_DIR/.git"
fi
if [ "$runtime_commit" != "$expected_commit" ]; then
  fail "LaTeXML commit mismatch: expected $expected_commit, got $runtime_commit"
fi
printf 'Runtime LaTeXML commit: %s\n' "$runtime_commit"

if [ -n "$expected_tag" ] && [ -d "$LATEXML_SOURCE_DIR/.git" ] && command_exists git; then
  runtime_tag="$(git -C "$LATEXML_SOURCE_DIR" describe --tags --exact-match HEAD 2>/dev/null || true)"
  if [ "$runtime_tag" != "$expected_tag" ]; then
    fail "LaTeXML tag mismatch: expected $expected_tag, got ${runtime_tag:-<none>}"
  fi
  printf 'Runtime LaTeXML tag: %s\n' "$runtime_tag"
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

cat > "$workdir/main.tex" <<'TEX'
\documentclass{article}
\begin{document}
\section{Smoke}
Inline math \(x^2\).
\end{document}
TEX

cd "$workdir"
"$LATEXMLC_BIN" --expire=-1 --format=html5 --destination=out.html --log=latexml.log main.tex
grep -q 'Smoke' out.html
grep -q 'Inline math' out.html
printf '%s\n' "LaTeXML runtime verification passed."

# A Rinspace work declares its own macros in its own *.sty files, so the runtime
# must be able to read them. Verify both halves: without --includestyles the
# project style is ignored, with it the project macro is typeset.
mkdir "$workdir/includestyles-smoke"
cat > "$workdir/includestyles-smoke/work.sty" <<'STY'
\newcommand{\RinProjectMacro}{\textbf{RinProjectStyleLoaded}}
STY
cat > "$workdir/includestyles-smoke/main.tex" <<'TEX'
\documentclass{article}
\usepackage{work}
\begin{document}
\RinProjectMacro
\end{document}
TEX
cd "$workdir/includestyles-smoke"
"$LATEXMLC_BIN" --expire=-1 --format=html5 --destination=without.html --log=without.log main.tex
if grep -q 'RinProjectStyleLoaded' without.html; then
  fail "--includestyles is missing or ignored: project style was loaded without the flag"
fi
"$LATEXMLC_BIN" --includestyles --expire=-1 --format=html5 --destination=with.html --log=with.log main.tex
grep -q 'RinProjectStyleLoaded' with.html
printf '%s\n' "LaTeXML project style verification passed."

mkdir "$workdir/texsvg-smoke"
cat > "$workdir/texsvg-smoke/diagram.tex" <<'TEX'
\documentclass{article}
\pagestyle{empty}
\usepackage{fontspec,tikz}
\begin{document}
\begin{tikzpicture}
\node at (0,0) {Rin SVG};
\draw (0,-0.25) -- (1,-0.25);
\end{tikzpicture}
\end{document}
TEX

cd "$workdir/texsvg-smoke"
if ! "$XELATEX_BIN" -interaction=nonstopmode -halt-on-error -no-shell-escape diagram.tex >xelatex.out 2>&1; then
  sed -n '1,120p' xelatex.out >&2
  if [ -f diagram.log ]; then
    tail -n 80 diagram.log >&2
  fi
  fail "XeLaTeX smoke conversion failed"
fi
if ! "$DVISVGM_BIN" --pdf --no-fonts --exact --bbox=min -n -o diagram.svg diagram.pdf >dvisvgm.out 2>&1; then
  sed -n '1,120p' dvisvgm.out >&2
  fail "dvisvgm smoke conversion failed"
fi
grep -q '<svg' diagram.svg
printf '%s\n' "TeX SVG runtime verification passed."
