import { readFile } from 'node:fs/promises';
import { chromium } from 'playwright-core';

const [executablePath, inputPath] = process.argv.slice(2);
const maxPDFBytes = 32 * 1024 * 1024;

async function print() {
  if (!executablePath || !inputPath || process.argv.length !== 4) {
    throw new Error('Expected the Chromium executable and prepared HTML input');
  }
  const html = await readFile(inputPath, 'utf8');
  const browser = await chromium.launch({
    executablePath,
    chromiumSandbox: true,
    headless: true,
    timeout: 15000,
    args: ['--renderer-process-limit=1', '--no-proxy-server'],
  });
  try {
    // A4 content width with the margins in publicationpdf/print.css.
    const context = await browser.newContext({
      offline: true,
      javaScriptEnabled: false,
      serviceWorkers: 'block',
      viewport: { width: 650, height: 972 },
    });
    let attemptedRequest = false;
    await context.route('**/*', (route) => {
      attemptedRequest = true;
      return route.abort();
    });
    const page = await context.newPage();
    await page.emulateMedia({ media: 'print' });
    await page.setContent(html, { waitUntil: 'load', timeout: 20000 });
    const assetsLoaded = await page.evaluate(async () => {
      await document.fonts.ready;
      await Promise.all(Array.from(document.images, (image) => image.decode()));
      return Array.from(document.fonts).every((font) => font.status !== 'error');
    });
    if (!assetsLoaded || attemptedRequest) {
      throw new Error('Print document requires unavailable assets');
    }

    const pdf = await page.pdf({
      preferCSSPageSize: true,
      printBackground: true,
      outline: true,
      tagged: true,
    });
    if (attemptedRequest || pdf.length > maxPDFBytes || pdf.subarray(0, 5).toString() !== '%PDF-') {
      throw new Error('Invalid PDF print output');
    }
    return pdf;
  } finally {
    await browser.close();
  }
}

try {
  process.stdout.write(await print());
} catch {
  // Playwright errors can contain document asset URLs and local paths.
  process.stderr.write('Markdown PDF printing failed\n');
  process.exitCode = 1;
}
