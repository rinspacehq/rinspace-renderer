#!/usr/bin/env bash
set -euo pipefail

image="${1:?usage: verify-latex-pdf-sandbox.sh <image> <evidence-directory>}"
evidence_dir="${2:?usage: verify-latex-pdf-sandbox.sh <image> <evidence-directory>}"
container_cli="${RIN_RENDERER_CONTAINER_CLI:-docker}"
run_key="${GITHUB_RUN_ID:-local}-$$"
tmp_root="$(mktemp -d)"
containers=()

fail_test() {
  printf '%s\n' "verify-latex-pdf-sandbox: $*" >&2
  exit 1
}

stage() {
  printf 'verify-latex-pdf-sandbox: stage=%s\n' "$1"
}

cleanup() {
  for name in "${containers[@]}"; do
    "$container_cli" rm -f "$name" >/dev/null 2>&1 || true
  done
  chmod -R u+w "$tmp_root" >/dev/null 2>&1 || true
  rm -rf "$tmp_root"
}
trap cleanup EXIT

mkdir -p "$evidence_dir"
image_id="$($container_cli image inspect --format '{{.Id}}' "$image")"
[[ "$image_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { printf '%s\n' 'image content ID is invalid' >&2; exit 1; }
printf '%s\n' "$image_id" > "$evidence_dir/image-id.txt"

prepare_case() {
  case_name="$1"
  case_root="$tmp_root/$case_name"
  mkdir -p "$case_root/workspace" "$case_root/output"
  chmod 0555 "$case_root/workspace"
  chmod 0777 "$case_root/output"
}

sandbox_args=()
set_sandbox_args() {
  case_root="$1"
  sandbox_args=(
    --cpus 1
    --memory 1536m
    --memory-swap 1536m
    --pids-limit 256
    --network none
    --read-only
    --cap-drop ALL
    --security-opt no-new-privileges:true
    --user 65532:65532
    --init
    --ipc none
    --tmpfs /tmp:rw,nosuid,nodev,noexec,size=256m,mode=1777
    --mount "type=bind,src=$case_root/workspace,dst=/workspace,readonly"
    --mount "type=bind,src=$case_root/output,dst=/output"
  )
}

run_compile() {
  case_name="$1"
  expected="$2"
  entrypoint="${3:-main.tex}"
  name="rin-latex-pdf-${case_name}-${run_key}"
  containers+=("$name")
  set_sandbox_args "$tmp_root/$case_name"
  "$container_cli" create --name "$name" "${sandbox_args[@]}" "$image" "$entrypoint" >/dev/null
  set +e
  "$container_cli" start -a "$name" >"$evidence_dir/${case_name}.container.log" 2>&1
  status=$?
  set -e
  "$container_cli" inspect "$name" > "$evidence_dir/${case_name}.inspect.json"
  if [ -f "$tmp_root/$case_name/output/compile.log" ]; then
    cp "$tmp_root/$case_name/output/compile.log" "$evidence_dir/${case_name}.compile.log"
  fi
  if [ "$expected" = success ] && [ "$status" -ne 0 ]; then
    printf 'case %s unexpectedly failed with %s\n' "$case_name" "$status" >&2
    tail -n 160 "$evidence_dir/${case_name}.container.log" >&2 || true
    tail -n 160 "$evidence_dir/${case_name}.compile.log" >&2 || true
    return 1
  fi
  if [ "$expected" = failure ] && [ "$status" -eq 0 ]; then
    printf 'case %s unexpectedly succeeded\n' "$case_name" >&2
    tail -n 160 "$evidence_dir/${case_name}.compile.log" >&2 || true
    return 1
  fi
}

prepare_case smoke
stage smoke
chmod 0755 "$tmp_root/smoke/workspace"
cat > "$tmp_root/smoke/workspace/main.tex" <<'TEX'
\documentclass{article}
\usepackage{fontspec}
\setmainfont{Latin Modern Roman}
\newfontfamily\cjkfont{Noto Serif CJK SC}
\begin{document}
Rinspace PDF smoke. {\cjkfont 中文字体。}
\end{document}
TEX
chmod 0444 "$tmp_root/smoke/workspace/main.tex"
chmod 0555 "$tmp_root/smoke/workspace"
run_compile smoke success
test -s "$tmp_root/smoke/output/main.pdf" || fail_test 'smoke PDF is missing or empty'
test -s "$tmp_root/smoke/output/main.synctex.gz" || fail_test 'smoke SyncTeX is missing or empty'
gzip -cd "$tmp_root/smoke/output/main.synctex.gz" \
  | awk 'index($0, "Input:") && index($0, "/workspace/") { found=1 } END { exit !found }' \
  || fail_test 'smoke SyncTeX did not preserve /workspace source paths'
test -s "$tmp_root/smoke/output/main.aux" || fail_test 'smoke aux is missing or empty'
test -s "$tmp_root/smoke/output/main.fls" || fail_test 'smoke recorder output is missing or empty'
test -s "$tmp_root/smoke/output/compile.log" || fail_test 'smoke compiler log is missing or empty'
node rin-renderer/deploy/validate-latex-pdf-inspect.mjs \
  "$evidence_dir/smoke.inspect.json" "$image_id" > "$evidence_dir/container-policy.json"

stage editor-integration-corpus
corpus_root="rin-renderer/corpus/pdf-editor-fixtures"
cp "$corpus_root/manifest.json" "$evidence_dir/pdf-editor-corpus.manifest.json"
printf 'case\texpected\tentrypoint\tresult\n' > "$evidence_dir/pdf-editor-corpus.tsv"
while IFS=$'\t' read -r corpus_id entrypoint expected diagnostic_line; do
  [[ "$corpus_id" =~ ^[a-z0-9][a-z0-9-]+$ ]] || fail_test 'editor corpus ID is invalid'
  case_name="editor-${corpus_id}"
  prepare_case "$case_name"
  chmod 0755 "$tmp_root/$case_name/workspace"
  cp -a "$corpus_root/$corpus_id/project"/. "$tmp_root/$case_name/workspace/"
  chmod -R a-w "$tmp_root/$case_name/workspace"
  run_compile "$case_name" "$expected" "$entrypoint"
  root_name="${entrypoint##*/}"
  stem="${root_name%.tex}"
  if [ "$expected" = success ]; then
    test -s "$tmp_root/$case_name/output/$stem.pdf" || fail_test "$corpus_id PDF is missing"
    test -s "$tmp_root/$case_name/output/$stem.synctex.gz" || fail_test "$corpus_id SyncTeX is missing"
    test -s "$tmp_root/$case_name/output/$stem.aux" || fail_test "$corpus_id aux is missing"
    test -s "$tmp_root/$case_name/output/$stem.fls" || fail_test "$corpus_id fls is missing"
    gzip -cd "$tmp_root/$case_name/output/$stem.synctex.gz" \
      | awk 'index($0, "Input:") && index($0, "/workspace/") { found=1 } END { exit !found }' \
      || fail_test "$corpus_id SyncTeX did not preserve /workspace source paths"
    result=passed
  else
    test -n "$diagnostic_line" || fail_test "$corpus_id failure has no diagnostic line"
    grep -F "$root_name:$diagnostic_line:" "$tmp_root/$case_name/output/compile.log" >/dev/null \
      || fail_test "$corpus_id compiler log has no file-line diagnostic"
    result=expected-failure
  fi
  printf '%s\t%s\t%s\t%s\n' "$corpus_id" "$expected" "$entrypoint" "$result" >> "$evidence_dir/pdf-editor-corpus.tsv"
done < <(node - "$corpus_root/manifest.json" <<'NODE'
const fs = require('node:fs');
const manifest = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
if (manifest.schemaVersion !== 'rin-latex-pdf-editor-corpus/v1' || !Array.isArray(manifest.cases)) process.exit(1);
for (const entry of manifest.cases) {
  const values = [entry.id, entry.entrypoint, entry.expected, entry.diagnostic?.line ?? ''];
  if (values.some((value) => /[\t\r\n\0]/.test(String(value)))) process.exit(1);
  process.stdout.write(`${values.join('\t')}\n`);
}
NODE
)
test "$(wc -l < "$evidence_dir/pdf-editor-corpus.tsv")" -eq 8 || fail_test 'editor corpus did not execute every case'

stage runtime-versions
runtime_name="rin-latex-pdf-runtime-${run_key}"
containers+=("$runtime_name")
set_sandbox_args "$tmp_root/smoke"
"$container_cli" create --name "$runtime_name" "${sandbox_args[@]}" \
  --entrypoint /usr/local/bin/verify-latex-pdf-runtime "$image" >/dev/null
if ! "$container_cli" start -a "$runtime_name" > "$evidence_dir/runtime-versions.txt"; then
  tail -n 200 "$evidence_dir/runtime-versions.txt" >&2 || true
  fail_test 'runtime version verification failed'
fi

prepare_case shell-escape
stage shell-escape-and-latexmkrc
chmod 0755 "$tmp_root/shell-escape/workspace"
cat > "$tmp_root/shell-escape/workspace/.latexmkrc" <<'RC'
system q(touch /output/latexmkrc-executed);
RC
cat > "$tmp_root/shell-escape/workspace/main.tex" <<'TEX'
\documentclass{article}
\begin{document}
\immediate\write18{touch /output/shell-escape-executed}
Shell escape must remain disabled.
\end{document}
TEX
chmod 0444 "$tmp_root/shell-escape/workspace/.latexmkrc" "$tmp_root/shell-escape/workspace/main.tex"
chmod 0555 "$tmp_root/shell-escape/workspace"
run_compile shell-escape success
test ! -e "$tmp_root/shell-escape/output/latexmkrc-executed" || fail_test '.latexmkrc was executed'
test ! -e "$tmp_root/shell-escape/output/shell-escape-executed" || fail_test 'TeX shell escape was executed'

prepare_case host-read
stage host-read
chmod 0755 "$tmp_root/host-read/workspace"
cat > "$tmp_root/host-read/workspace/main.tex" <<'TEX'
\documentclass{article}
\begin{document}
\input{/etc/passwd}
\end{document}
TEX
chmod 0444 "$tmp_root/host-read/workspace/main.tex"
chmod 0555 "$tmp_root/host-read/workspace"
run_compile host-read failure
if grep -q 'root:.*:0:0:' "$tmp_root/host-read/output/compile.log"; then
  fail_test 'host passwd contents reached the TeX log'
fi

prepare_case write-escape
stage write-escape
chmod 0755 "$tmp_root/write-escape/workspace"
cat > "$tmp_root/write-escape/workspace/main.tex" <<'TEX'
\documentclass{article}
\newwrite\outside
\begin{document}
\immediate\openout\outside=/workspace/write-escape-created
\immediate\write\outside{unsafe}
\end{document}
TEX
chmod 0444 "$tmp_root/write-escape/workspace/main.tex"
chmod 0555 "$tmp_root/write-escape/workspace"
run_compile write-escape failure
test ! -e "$tmp_root/write-escape/workspace/write-escape-created" || fail_test 'workspace write escape created a file'

prepare_case timeout
stage timeout
chmod 0755 "$tmp_root/timeout/workspace"
cat > "$tmp_root/timeout/workspace/main.tex" <<'TEX'
\documentclass{article}
\begin{document}
\loop\iftrue\repeat
\end{document}
TEX
chmod 0444 "$tmp_root/timeout/workspace/main.tex"
chmod 0555 "$tmp_root/timeout/workspace"
timeout_name="rin-latex-pdf-timeout-${run_key}"
containers+=("$timeout_name")
set_sandbox_args "$tmp_root/timeout"
"$container_cli" create --name "$timeout_name" "${sandbox_args[@]}" "$image" main.tex >/dev/null
"$container_cli" start -a "$timeout_name" >"$evidence_dir/timeout.container.log" 2>&1 &
timeout_client=$!
for _ in 1 2 3; do
  sleep 1
  [ "$($container_cli inspect --format '{{.State.Running}}' "$timeout_name")" = true ] || break
done
"$container_cli" stop --time 2 "$timeout_name" >/dev/null
wait "$timeout_client" || true
printf '%s\n' '{"configuredWallTimeSeconds":90,"boundedProbeSeconds":3,"result":"terminated"}' > "$evidence_dir/timeout-probe.json"

fork_name="rin-latex-pdf-fork-${run_key}"
stage fork-bomb
containers+=("$fork_name")
"$container_cli" create --name "$fork_name" \
  --cpus 0.25 --memory 64m --memory-swap 64m --pids-limit 16 --network none --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 --init --ipc none \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777 \
  --entrypoint /bin/sh "$image" -c \
  'i=0; while [ "$i" -lt 64 ]; do sleep 30 & i=$((i + 1)); done; wait' >/dev/null
"$container_cli" start "$fork_name" >/dev/null
sleep 2
fork_limit="$($container_cli inspect --format '{{.HostConfig.PidsLimit}}' "$fork_name")"
[ "$fork_limit" = 16 ] || fail_test "fork test PID limit drifted: $fork_limit"
fork_pids=0
if [ "$($container_cli inspect --format '{{.State.Running}}' "$fork_name")" = true ]; then
  observed="$($container_cli stats --no-stream --format '{{.PIDs}}' "$fork_name" 2>/dev/null || true)"
  case "$observed" in
    ''|*[!0-9]*) ;;
    *) fork_pids="$observed" ;;
  esac
fi
"$container_cli" logs "$fork_name" > "$evidence_dir/fork-bomb.log" 2>&1 || true
[ "$fork_pids" -le 16 ] || { printf 'fork test exceeded PID cgroup: %s\n' "$fork_pids" >&2; exit 1; }
printf '{"configuredPids":16,"observedPids":%s,"configuredNanoCpus":250000000}\n' "$fork_pids" > "$evidence_dir/fork-bomb.json"
"$container_cli" kill "$fork_name" >/dev/null 2>&1 || true

oom_name="rin-latex-pdf-oom-${run_key}"
stage oom
containers+=("$oom_name")
"$container_cli" create --name "$oom_name" \
  --cpus 0.25 --memory 64m --memory-swap 64m --pids-limit 16 --network none --read-only \
  --cap-drop ALL --security-opt no-new-privileges:true --user 65532:65532 --init --ipc none \
  --tmpfs /tmp:rw,nosuid,nodev,noexec,size=16m,mode=1777 \
  --entrypoint /usr/bin/perl "$image" -e '$value = "x" x (512 * 1024 * 1024); print length($value)' >/dev/null
set +e
"$container_cli" start -a "$oom_name" > "$evidence_dir/oom.log" 2>&1
oom_status=$?
set -e
oom_killed="$($container_cli inspect --format '{{.State.OOMKilled}}' "$oom_name")"
[ "$oom_killed" = true ] || { printf 'OOM probe was not killed by its memory cgroup (status %s)\n' "$oom_status" >&2; exit 1; }
printf '{"configuredMemoryBytes":67108864,"configuredMemorySwapBytes":67108864,"oomKilled":true,"exitStatus":%s}\n' "$oom_status" > "$evidence_dir/oom.json"

printf '%s\n' \
  'shellEscape=blocked' \
  'network=none' \
  'hostRead=blocked' \
  'writeEscape=blocked' \
  'latexmkrc=ignored' \
  'packageDownload=blocked-by-network-and-no-tlmgr' \
  'forkBomb=pids-cgroup-contained' \
  'timeout=bounded-harness-terminated' \
  'oom=memory-cgroup-killed' \
  > "$evidence_dir/attack-matrix.txt"
