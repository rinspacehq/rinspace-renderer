// Ordinary, credential-free CI input preparation. Nothing is published.
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { execFileSync } from 'node:child_process';
import path from 'node:path';

const [output] = process.argv.slice(2);
if (!output || !path.isAbsolute(output)) throw new Error('Absolute new test-runtime directory required');
const lock = JSON.parse(await readFile(new URL('./typst-html-toolchain.lock.json', import.meta.url), 'utf8'));
await mkdir(output, { mode: 0o700 });
async function download(url, filename, expected, maximum) {
  if (!url.startsWith('https://') || !/^[a-f0-9]{64}$/.test(expected)) throw new Error('Unpinned input');
  const destination = path.join(output, filename);
  await writeFile(destination, '', { flag: 'wx', mode: 0o600 });
  // Match the existing toolchain builder and honor the caller's normal HTTPS
  // proxy configuration (Node 22 fetch does not do so automatically).
  execFileSync('curl', ['--fail', '--location', '--silent', '--show-error', '--proto', '=https', '--proto-redir', '=https',
    '--tlsv1.2', '--connect-timeout', '15', '--max-time', '120', '--retry', '2', '--max-filesize', String(maximum), '--output', destination, url]);
  const body = await readFile(destination);
  if (body.length > maximum || createHash('sha256').update(body).digest('hex') !== expected) throw new Error('Locked input hash or size mismatch');
}
await download(`https://github.com/typst/typst/releases/download/v${lock.compiler.version}/${lock.compiler.asset}`, 'typst.tar.xz', lock.compiler.assetSha256, 64 << 20);
await download(lock.font.release, 'font.deb', lock.font.packageSha256, 16 << 20);
execFileSync('tar', ['-xJf', path.join(output, 'typst.tar.xz'), '-C', output, '--strip-components=1', 'typst-x86_64-unknown-linux-musl/typst']);
execFileSync('dpkg-deb', ['-x', path.join(output, 'font.deb'), path.join(output, 'font')]);
const binary = path.join(output, 'typst');
const font = path.join(output, 'font', lock.font.debPath);
for (const [file, expected] of [[binary, lock.compiler.binarySha256], [font, lock.font.sha256]]) {
  if (createHash('sha256').update(await readFile(file)).digest('hex') !== expected) throw new Error('Extracted test runtime hash mismatch');
}
// Only validated paths, not remote-controlled metadata, reach GITHUB_ENV.
if (/[\r\n]/.test(binary + font)) throw new Error('Unsafe environment path');
console.log(`RIN_TYPST_TEST_BINARY=${binary}\nRIN_TYPST_TEST_FONT=${font}`);
