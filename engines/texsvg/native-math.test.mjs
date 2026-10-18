import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';

// Run in the reviewed runtime image: real TeX + dvisvgm, no network, account,
// project files or object storage required. Unit tests also cover malformed
// wrappers; this contract proves the actual worker can compile its own output.
const worker = spawn(process.env.RIN_TEXSVG_TEST_BIN || '/usr/local/bin/rin-renderer-texsvg', [], {
  env: { ...process.env, RIN_RENDERER_ADDR: '127.0.0.1:18092' }, stdio: ['ignore', 'ignore', 'pipe'],
});
let stderr = '';
worker.stderr.on('data', (chunk) => { stderr = (stderr + chunk.toString()).slice(-8192); });
try {
  let ready = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    try {
      const response = await fetch('http://127.0.0.1:18092/health');
      if (response.ok) {
        assert.equal((await response.json()).mathWrapperVersion, 'rin-texsvg-math-display/v2');
        ready = true; break;
      }
    } catch (error) { if (error?.code === 'ERR_ASSERTION') throw error; }
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  assert(ready, `Native worker was not ready: ${stderr}`);
  const sources = [
    String.raw`\begin{equation}\bm{x}=\frac{1}{2}\end{equation}`,
    String.raw`\begin{equation*}\bm{x}=\frac{1}{2}\end{equation*}`,
    String.raw`\begin{align*}a&=b+c\\d&=e+f\end{align*}`,
    String.raw`\begin{alignat*}{2}a&=b & c&=d\\e&=f & g&=h\end{alignat*}`,
    String.raw`\begin{gather*}a=b\\c=d\end{gather*}`,
    String.raw`\begin{multline*}a+b+c\\=d+e\end{multline*}`,
    String.raw`\begin{flalign*}a&=b & c&=d\\e&=f & g&=h\end{flalign*}`,
    String.raw`\begin{displaymath}\bm{x}+\bm{y}\end{displaymath}`,
    String.raw`\begin{cases}x^2 & x>0\\0 & x\le0\end{cases}`,
    String.raw`\begin{aligned}a&=b\\c&=d\end{aligned}`,
    String.raw`x+y`,
  ];
  for (const source of sources) {
    const response = await fetch('http://127.0.0.1:18092/math', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ type: 'math', options: 'display', body: source }),
      signal: AbortSignal.timeout(40000),
    });
    const result = await response.json();
    assert.equal(response.status, 200, result.message);
    assert.match(result.svg, /<svg[\s>]/);
    assert.ok(!result.svg.includes('<script'));
  }
  for (const source of [String.raw`\begin{equation}x`, String.raw`\definitelyunsupportedcommand{x}`]) {
    const response = await fetch('http://127.0.0.1:18092/math', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ type: 'math', options: 'display', body: source }),
      signal: AbortSignal.timeout(40000),
    });
    assert.equal(response.status, 502, 'Invalid TeX must remain a failure');
    assert.equal((await response.json()).svg, undefined);
  }
  console.log(`Native TeXSVG: ${sources.length} actual TeX/SVG cases and 2 invalid-source cases passed`);
} finally { worker.kill('SIGTERM'); }
