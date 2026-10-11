// A browser test of the web UI: the real evalsid serves the real files to a
// real Chromium. Go tests cover the headers and the served bytes; this covers
// what only a browser does: run the script under the Content-Security-Policy,
// render the pages, and sign in.
//
//   EVALSI_UI_WORKER   the worker command, e.g. "python/.venv/bin/python -m evalsi"
//   CHROMIUM_PATH      a Chromium binary, if playwright-core has none installed
//   EVALSID            a built evalsid; by default the test builds one
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import net from 'node:net';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const workerCommand = (process.env.EVALSI_UI_WORKER ?? 'python -m evalsi').split(' ');
const scratch = mkdtempSync(path.join(tmpdir(), 'evalsi-ui-'));
const servers = [];
let browser;
let evalsid = process.env.EVALSID;

const freePort = () => new Promise((resolve, reject) => {
  const probe = net.createServer();
  probe.listen(0, '127.0.0.1', () => {
    const { port } = probe.address();
    probe.close(() => resolve(port));
  });
  probe.on('error', reject);
});

async function startServer(name, extraConfig = '') {
  const port = await freePort();
  const config = path.join(scratch, `${name}.yaml`);
  writeFileSync(config, [
    `listen: 127.0.0.1:${port}`,
    `data_dir: ${path.join(scratch, name)}`,
    'worker:',
    `  command: ${JSON.stringify(workerCommand)}`,
    extraConfig,
  ].join('\n'));
  const child = spawn(evalsid, ['serve', '-config', config, ...(extraConfig ? [] : ['-no-auth'])], { stdio: ['ignore', 'pipe', 'pipe'] });
  let log = '';
  child.stdout.on('data', chunk => { log += chunk; });
  child.stderr.on('data', chunk => { log += chunk; });
  servers.push(child);
  const url = `http://127.0.0.1:${port}`;
  for (let attempt = 0; attempt < 150; attempt += 1) {
    if (child.exitCode !== null) assert.fail(`evalsid exited early:\n${log}`);
    try {
      if ((await fetch(`${url}/ui/`)).ok) return url;
    } catch { /* not up yet */ }
    await new Promise(resolve => setTimeout(resolve, 200));
  }
  assert.fail(`evalsid did not start:\n${log}`);
}

// problems collects what a user would see broken: script errors, console
// errors, failed requests, and anything the Content-Security-Policy blocked.
function watch(page) {
  const problems = [];
  page.on('pageerror', error => problems.push(`pageerror: ${error.message}`));
  page.on('console', message => {
    if (message.type() === 'error') problems.push(`console: ${message.text()}`);
  });
  page.on('requestfailed', request => problems.push(`request failed: ${request.url()}`));
  page.on('response', response => {
    if (response.status() >= 400) problems.push(`HTTP ${response.status()}: ${response.url()}`);
  });
  page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', event => {
      console.error(`CSP blocked ${event.violatedDirective} ${event.blockedURI}`);
    });
  });
  return problems;
}

test.before(async () => {
  if (!evalsid) {
    evalsid = path.join(scratch, 'evalsid');
    execFileSync('go', ['build', '-o', evalsid, './cmd/evalsid'], { cwd: root, stdio: 'inherit' });
  }
  const executablePath = process.env.CHROMIUM_PATH;
  browser = await chromium.launch(executablePath && existsSync(executablePath) ? { executablePath } : {});
});

test.after(async () => {
  await browser?.close();
  for (const child of servers) child.kill();
  rmSync(scratch, { recursive: true, force: true });
});

test('the UI loads under its Content-Security-Policy and every section renders', async () => {
  const url = await startServer('open');
  const page = await browser.newPage();
  const problems = watch(page);
  await page.goto(`${url}/ui/`);
  await page.waitForSelector('#project option', { state: 'attached' });
  assert.match(await page.textContent('#whoami'), /anonymous|auth off/);
  assert.equal(await page.locator('#signout').isHidden(), true, 'no sign-out when auth is off');

  // The inline-script protection holds: a script the page did not ship does not run.
  const injected = await page.evaluate(() => {
    const script = document.createElement('script');
    script.textContent = 'window.__ran = true';
    document.head.appendChild(script);
    return window.__ran === true;
  });
  assert.equal(injected, false, 'an inline script ran despite the CSP');
  problems.length = 0; // that attempt is expected to be blocked

  const sections = {
    runs: /Runs/i, compare: /Compare|baseline/i, policies: /Online policies|policies/i,
    queues: /Annotation|queues/i, guardrails: /Guardrails/i, catalog: /Evaluator catalog/i,
  };
  for (const [section, heading] of Object.entries(sections)) {
    await page.click(`#nav a[data-section="${section}"]`);
    await page.waitForFunction(name => document.querySelector(`#nav a[data-section="${name}"]`)?.classList.contains('active'), section);
    // The nav link turns active before its view has loaded, and the page may render
    // a view again (a project list arriving re-runs the route). Wait for the heading
    // itself, then assert on it.
    await page
      .waitForFunction(([source, flags]) => new RegExp(source, flags).test(document.querySelector('#main')?.textContent ?? ''), [heading.source, heading.flags])
      .catch(() => {});
    assert.match(await page.textContent('#main'), heading, `section ${section}`);
  }

  // The catalog lists the built-in evaluators and its filter narrows them.
  await page.click('#nav a[data-section="catalog"]');
  await page.waitForSelector('#main table tbody tr');
  const all = await page.locator('#main table tbody tr').count();
  assert.ok(all > 10, `only ${all} evaluators listed`);
  await page.fill('input[aria-label="filter evaluators"]', 'exact-match');
  const narrowed = await page.locator('#main table tbody tr').count();
  assert.ok(narrowed >= 1 && narrowed < all, `the filter left ${narrowed} of ${all}`);
  assert.deepEqual(problems, []);
  await page.close();
});

test('with authentication on, the UI asks for a credential and accepts only a valid one', async () => {
  const key = execFileSync(evalsid, ['auth', 'new-key'], { encoding: 'utf8' });
  const secret = key.match(/key:\s+(\S+)/)[1];
  const hash = key.match(/hash:\s+(\S+)/)[1];
  const url = await startServer('auth', [
    'auth:', '  api_keys:', '    keys:',
    `      - {name: ui-test, key: "${hash}", roles: {"*": [owner]}}`,
  ].join('\n'));
  const page = await browser.newPage();
  const problems = watch(page);
  await page.goto(`${url}/ui/`);
  await page.waitForSelector('.signin');
  assert.equal(await page.locator('#whoami').textContent(), '');

  const fillAndSubmit = async value => {
    await page.fill('input[aria-label="API key or token"]', value);
    await page.click('.signin button[type="submit"]');
  };
  await fillAndSubmit('evk_not-a-real-key');
  await page.waitForSelector('.signin .error');
  assert.match(await page.textContent('.signin .error'), /not accepted/i);

  await fillAndSubmit(secret);
  await page.waitForFunction(() => document.getElementById('whoami').textContent.trim() !== '');
  assert.match(await page.textContent('#whoami'), /ui-test/);
  assert.equal(await page.locator('.signin').count(), 0);
  assert.ok(await page.locator('#project option').count() >= 1);

  // The credential lives in this tab's sessionStorage only, and signing out forgets it.
  assert.equal(await page.evaluate(() => sessionStorage.getItem('evalsi.token')), secret);
  assert.equal(await page.evaluate(() => localStorage.getItem('evalsi.token')), null);
  await page.click('#signout');
  await page.waitForSelector('.signin');
  assert.equal(await page.evaluate(() => sessionStorage.getItem('evalsi.token')), null);

  // The 401 from the wrong key is the one expected failed request; nothing else.
  const unexpected = problems.filter(problem => !/401|Unauthorized/i.test(problem));
  assert.deepEqual(unexpected, []);
  await page.close();
});
