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
import { createHash, createSign, generateKeyPairSync, randomBytes } from 'node:crypto';
import http from 'node:http';
import { mkdtempSync, writeFileSync, rmSync, existsSync } from 'node:fs';
import net from 'node:net';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright-core';
import axe from 'axe-core';

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
// Await it before navigating: the CSP listener must be installed first.
async function watch(page) {
  const problems = [];
  page.on('pageerror', error => problems.push(`pageerror: ${error.message}`));
  page.on('console', message => {
    if (message.type() === 'error') problems.push(`console: ${message.text()}`);
  });
  page.on('requestfailed', request => problems.push(`request failed: ${request.url()}`));
  page.on('response', response => {
    if (response.status() >= 400) problems.push(`HTTP ${response.status()}: ${response.url()}`);
  });
  await page.addInitScript(() => {
    document.addEventListener('securitypolicyviolation', event => {
      console.error(`CSP blocked ${event.violatedDirective} ${event.blockedURI}`);
    });
  });
  return problems;
}

async function checkAccessibility(page) {
  await page.evaluate(axe.source);
  const violations = await page.evaluate(async () => {
    const result = await window.axe.run(document, {
      runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa'] },
    });
    return result.violations.map(v => ({ id: v.id, impact: v.impact,
      nodes: v.nodes.map(n => ({ target: n.target, summary: n.failureSummary })) }));
  });
  assert.deepEqual(violations, [], `accessibility: ${page.url()}`);
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
  const problems = await watch(page);
  await page.goto(`${url}/ui/`);
  await page.waitForSelector('#project option', { state: 'attached' });
  assert.match(await page.textContent('#whoami'), /anonymous|auth off/);
  assert.equal(await page.locator('#signout').isHidden(), true, 'no sign-out when auth is off');

  // The inline-script protection holds: a script the page did not ship does not run.
  const blocked = page.waitForEvent('console', { predicate: m => m.text().includes('CSP blocked script-src-elem inline') });
  const injected = await page.evaluate(() => {
    const script = document.createElement('script');
    script.textContent = 'window.__ran = true';
    document.head.appendChild(script);
    return window.__ran === true;
  });
  assert.equal(injected, false, 'an inline script ran despite the CSP');
  await blocked;
  problems.length = 0; // that attempt is expected to be blocked

  const sections = {
    runs: /Runs/i, compare: /Compare|baseline/i, policies: /Online policies|policies/i,
    queues: /Annotation|queues/i, guardrails: /Guardrails/i, catalog: /Evaluator catalog/i,
    traces: /Production traces/i, inbox: /Failure inbox/i, datasets: /Dataset versions/i, releases: /Release decisions/i,
  };
  for (const [section, heading] of Object.entries(sections)) {
    await page.click(`#nav a[data-section="${section}"]`);
    await page.waitForFunction(name => document.querySelector(`#nav a[data-section="${name}"]`)?.classList.contains('active'), section);
    // The nav link turns active before its view has loaded, and the page may render
    // a view again after it showed (the first click turns an empty hash into
    // #/runs, whose hashchange re-runs the route and shows "Loading…" again).
    // Assert on the text the wait saw, not on a second read that can land on
    // such a re-render.
    const seen = await page
      .waitForFunction(([source, flags]) => {
        const text = document.querySelector('#main')?.textContent ?? '';
        return new RegExp(source, flags).test(text) && text;
      }, [heading.source, heading.flags])
      .then(handle => handle.jsonValue())
      .catch(() => page.textContent('#main'));
    assert.match(seen, heading, `section ${section}`);
    await checkAccessibility(page);
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
  const problems = await watch(page);
  await page.goto(`${url}/ui/`);
  await page.waitForSelector('.signin');
  await checkAccessibility(page);
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
  // whoami is set before the first view replaces the sign-in card.
  await page.waitForSelector('.signin', { state: 'detached' });
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

test('an annotator answers and skips items in the browser', async () => {
  const url = await startServer('annotate');
  const call = async (method, route, body) => {
    const resp = await fetch(`${url}${route}`, {
      method, headers: { 'Content-Type': 'application/json' }, body: body ? JSON.stringify(body) : undefined,
    });
    const text = await resp.text();
    assert.ok(resp.ok, `${method} ${route}: ${resp.status} ${text}`);
    return text ? JSON.parse(text) : {};
  };
  await call('POST', '/v1alpha1/queues', {
    name: 'helpfulness', project: 'default', instructions: 'Judge the reply, not the tone.',
    questions: [
      { name: 'good', prompt: 'Is the reply good?', kind: 'QUESTION_KIND_PASS_FAIL' },
      { name: 'score', prompt: 'How helpful?', kind: 'QUESTION_KIND_SCORE', min: 1, max: 5 },
      { name: 'kind', prompt: 'What kind of reply?', kind: 'QUESTION_KIND_LABEL', options: ['answer', 'refusal'] },
      { name: 'notes', prompt: 'Notes', kind: 'QUESTION_KIND_TEXT', optional: true },
    ],
  });
  await call('POST', '/v1alpha1/queues/helpfulness/items', {
    project: 'default',
    records: { records: [
      { id: 'r1', input: { text: 'What is 2+2?' }, output: { text: '<b>4</b>' } },
      { id: 'r2', input: { text: 'Capital of France?' }, output: { text: 'Paris' },
        trajectory: { steps: [{ type: 'STEP_TYPE_TOOL', name: 'search', input: { text: 'france capital' }, output: { text: `${'long '.repeat(200)}END` } }] } },
    ] },
  });

  const page = await browser.newPage();
  const problems = await watch(page);
  await page.goto(`${url}/ui/#/queues/helpfulness/annotate`);
  await page.waitForSelector('form.annotate');
  await checkAccessibility(page);
  assert.match(await page.textContent('#main'), /Judge the reply, not the tone\./);
  // Content is text, never markup.
  assert.match(await page.textContent('#main'), /<b>4<\/b>/);
  assert.equal(await page.locator('#main b').count(), 0);

  // An incomplete form is not sent.
  await page.click('form.annotate button[type="submit"]');
  assert.match(await page.textContent('form.annotate .error'), /an answer is needed/);

  await page.check('input[value="pass"]');
  await page.fill('input[type="number"]', '9');
  await page.check('input[value="answer"]');
  await page.click('form.annotate button[type="submit"]');
  assert.match(await page.textContent('form.annotate .error'), /from 1 to 5/);
  await page.fill('input[type="number"]', '4');
  await page.fill('textarea[aria-label="comment"]', 'looks right');
  await page.click('form.annotate button[type="submit"]');

  // The second item has a long tool result in its trajectory, shown whole.
  await page.waitForSelector('text=trajectory: 1 step');
  await page.click('summary:has-text("trajectory")');
  assert.match(await page.textContent('form.annotate details'), /long long long.*END/s);

  // The second item is skipped, which leaves nothing for this annotator.
  await page.waitForFunction(() => document.querySelector('form.annotate .sub')?.textContent.includes('1 answered'));
  await page.click('text=Skip this item');
  await page.waitForSelector('text=Nothing left for you');

  const { annotations } = await call('GET', '/v1alpha1/queues/helpfulness/annotations?project=default');
  const answered = annotations.filter(a => !a.skipped);
  assert.equal(annotations.length, 2);
  assert.equal(answered.length, 1);
  assert.equal(answered[0].comment, 'looks right');
  assert.deepEqual(answered[0].answers.map(a => a.question), ['good', 'score', 'kind']);
  assert.equal(answered[0].answers[1].number, 4);
  assert.deepEqual(problems, []);
  await page.close();
});

// A minimal OpenID provider: authorization code with PKCE, ES256 ID tokens and a
// JWKS, on a loopback port. It checks what a real provider would: the S256
// challenge matches the verifier, and a code works once.
async function startProvider() {
  const port = await freePort();
  const issuer = `http://127.0.0.1:${port}`;
  const { publicKey, privateKey } = generateKeyPairSync('ec', { namedCurve: 'P-256' });
  const b64 = value => Buffer.from(value).toString('base64url');
  const codes = new Map();
  const provider = { issuer, deny: false, tokenRequests: [], authorizeRequests: [] };
  const idToken = () => {
    const now = Math.floor(Date.now() / 1000);
    const head = b64(JSON.stringify({ alg: 'ES256', kid: 'k1', typ: 'JWT' }));
    const body = b64(JSON.stringify({
      iss: issuer, aud: 'evalsi-ui', sub: 'alice', email: 'alice@example.com', name: 'Alice', iat: now, exp: now + 600,
    }));
    const sig = createSign('SHA256').update(`${head}.${body}`).sign({ key: privateKey, dsaEncoding: 'ieee-p1363' });
    return `${head}.${body}.${b64(sig)}`;
  };
  const server = http.createServer((req, res) => {
    const url = new URL(req.url, issuer);
    const json = (status, value) => { res.writeHead(status, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(value)); };
    if (url.pathname === '/.well-known/openid-configuration') {
      return json(200, { issuer, authorization_endpoint: `${issuer}/authorize`, token_endpoint: `${issuer}/token`, jwks_uri: `${issuer}/jwks` });
    }
    if (url.pathname === '/jwks') return json(200, { keys: [{ ...publicKey.export({ format: 'jwk' }), kid: 'k1', alg: 'ES256', use: 'sig' }] });
    if (url.pathname === '/authorize') {
      const q = url.searchParams;
      provider.authorizeRequests.push(Object.fromEntries(q));
      const back = new URL(q.get('redirect_uri'));
      back.searchParams.set('state', q.get('state'));
      if (provider.deny) {
        back.searchParams.set('error', 'access_denied');
        back.searchParams.set('error_description', 'not for you');
      } else {
        assert.equal(q.get('response_type'), 'code');
        assert.equal(q.get('code_challenge_method'), 'S256');
        const code = randomBytes(8).toString('hex');
        codes.set(code, { challenge: q.get('code_challenge'), redirect: q.get('redirect_uri'), client: q.get('client_id') });
        back.searchParams.set('code', code);
      }
      res.writeHead(302, { Location: back.toString() });
      return res.end();
    }
    if (url.pathname === '/token' && req.method === 'POST') {
      let raw = '';
      req.on('data', chunk => { raw += chunk; });
      req.on('end', () => {
        const form = new URLSearchParams(raw);
        provider.tokenRequests.push(Object.fromEntries(form));
        const entry = codes.get(form.get('code'));
        codes.delete(form.get('code'));
        const verifier = createHash('sha256').update(form.get('code_verifier') ?? '').digest('base64url');
        if (!entry || entry.challenge !== verifier || entry.redirect !== form.get('redirect_uri') || entry.client !== form.get('client_id')) {
          return json(400, { error: 'invalid_grant', error_description: 'bad code or verifier' });
        }
        return json(200, { id_token: idToken(), access_token: 'opaque-access', refresh_token: 'r', token_type: 'Bearer', expires_in: 600 });
      });
      return undefined;
    }
    res.writeHead(404);
    return res.end();
  });
  await new Promise(resolve => server.listen(port, '127.0.0.1', resolve));
  servers.push({ kill: () => server.close() });
  return provider;
}

test('the UI signs in through the identity provider with PKCE', async () => {
  const idp = await startProvider();
  const url = await startServer('oidc', [
    'auth:', '  jwt:', '    providers:',
    `      - {name: idp, issuer: "${idp.issuer}", audiences: [evalsi-ui], jwks: {url: "${idp.issuer}/jwks", allow_insecure: true}}`,
    `  cli_login: {issuer: "${idp.issuer}", client_id: evalsi-cli, ui_client_id: evalsi-ui, scopes: [openid, email, offline_access]}`,
  ].join('\n'));
  const page = await browser.newPage();
  const problems = await watch(page);
  await page.goto(`${url}/ui/#/policies`);
  await page.waitForSelector('#sso');

  await page.click('#sso');
  await page.waitForFunction(() => document.getElementById('whoami').textContent.includes('alice'));
  // Back on the page it left, with the one-time code out of the address bar.
  const here = new URL(page.url());
  assert.equal(here.search, '');
  assert.equal(here.hash, '#/policies');
  assert.equal(await page.evaluate(() => sessionStorage.getItem('evalsi.login')), null);
  const token = await page.evaluate(() => sessionStorage.getItem('evalsi.token'));
  assert.equal(token.split('.').length, 3, 'the ID token is the credential');
  assert.equal(await page.evaluate(() => localStorage.getItem('evalsi.token')), null);

  // What the browser asked the provider for, and what evalsid exchanged.
  const ask = idp.authorizeRequests[0];
  assert.equal(ask.client_id, 'evalsi-ui');
  assert.equal(ask.redirect_uri, `${url}/ui/`);
  assert.equal(ask.scope, 'openid email', 'a browser tab asks for no refresh token');
  assert.match(ask.code_challenge, /^[A-Za-z0-9_-]{43}$/);
  assert.equal(idp.tokenRequests[0].grant_type, 'authorization_code');
  assert.equal(idp.tokenRequests[0].client_secret, undefined);

  // Signing out forgets it, and signing in again works.
  await page.click('#signout');
  await page.waitForSelector('#sso');
  assert.equal(await page.evaluate(() => sessionStorage.getItem('evalsi.token')), null);

  // A provider that refuses is reported, not retried.
  idp.deny = true;
  await page.click('#sso');
  await page.waitForSelector('.signin .error');
  assert.match(await page.textContent('.signin .error'), /refused the sign-in: not for you/);

  // A forged error is not shown as the provider's word, and a forged response
  // does not cancel a login in progress.
  idp.deny = false;
  await page.goto(`${url}/ui/?error=access_denied&error_description=Call+support+on+555&state=forged`);
  await page.waitForSelector('.signin .error');
  assert.match(await page.textContent('.signin .error'), /did not match/);
  assert.doesNotMatch(await page.textContent('#main'), /555/);

  // A response this tab did not ask for is not trusted.
  await page.goto(`${url}/ui/?code=planted&state=forged`);
  await page.waitForSelector('.signin .error');
  assert.match(await page.textContent('.signin .error'), /did not match/);
  assert.equal(await page.evaluate(() => sessionStorage.getItem('evalsi.token')), null);

  // The pasted-token path stays available.
  await page.locator('.signin summary').click();
  assert.equal(await page.locator('input[aria-label="API key or token"]').isVisible(), true);
  const unexpected = problems.filter(problem => !/40[13]|Unauthorized|Forbidden/i.test(problem));
  assert.deepEqual(unexpected, []);
  await page.close();
});

test('keyboard navigation and both palettes pass automated WCAG checks', async () => {
  const url = await startServer('keyboard-accessibility');
  const page = await browser.newPage();
  await page.goto(`${url}/ui/`);
  await page.waitForSelector('#project option', { state: 'attached' });
  await page.keyboard.press('Tab');
  assert.equal(await page.locator(':focus').getAttribute('class'), 'brand');
  for (const section of ['runs', 'compare', 'policies', 'traces', 'inbox', 'datasets', 'releases', 'queues', 'guardrails', 'catalog']) {
    await page.keyboard.press('Tab');
    assert.equal(await page.locator(':focus').getAttribute('data-section'), section);
    await page.keyboard.press('Enter');
    await page.waitForFunction(() => !document.getElementById('main').textContent.includes('Loading'));
    assert.equal(new URL(page.url()).hash, `#/${section}`);
    await checkAccessibility(page);
  }
  await page.emulateMedia({ colorScheme: 'dark' });
  for (const section of ['runs', 'compare', 'policies', 'traces', 'inbox', 'datasets', 'releases', 'queues', 'guardrails', 'catalog']) {
    await page.click(`#nav a[data-section="${section}"]`);
    await page.waitForFunction(() => !document.getElementById('main').textContent.includes('Loading'));
    await checkAccessibility(page);
  }
  await page.close();
});

test('a production incident is edited, reviewed and exported through the inbox', async () => {
  const url = await startServer('failure-inbox');
  const traceId = randomBytes(16).toString('hex');
  const now = String(BigInt(Date.now()) * 1000000n);
  const sent = await fetch(`${url}/v1/traces`, {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ resourceSpans: [{
      resource: { attributes: [{ key: 'service.name', value: { stringValue: 'support-agent' } }] },
      scopeSpans: [{ spans: [{ traceId, spanId: randomBytes(8).toString('hex'), name: 'refund',
        startTimeUnixNano: now, endTimeUnixNano: now,
        attributes: [
          { key: 'input.value', value: { stringValue: 'Refund a pending order' } },
          { key: 'output.value', value: { stringValue: 'Refunded without approval' } },
        ],
      }] }],
    }] }),
  });
  assert.equal(sent.status, 200);
  for (let attempt = 0; attempt < 100; attempt += 1) {
    const trace = await fetch(`${url}/v1alpha1/traces/${traceId}?project=default`);
    if (trace.ok) break;
    assert.ok(attempt < 99, 'trace did not assemble');
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  const page = await browser.newPage();
  const problems = await watch(page);
  await page.goto(`${url}/ui/#/traces/${traceId}`);
  await page.getByLabel('Case title', { exact: true }).fill('<script>alert("incident")</script> Refund');
  await page.getByLabel('What went wrong?', { exact: true }).fill('Refund requires approval');
  await page.getByRole('button', { name: 'Capture for review', exact: true }).click();
  await page.getByRole('heading', { name: 'Edit case', exact: true }).waitFor();
  assert.equal(await page.locator('#main script').count(), 0, 'incident text was interpreted as HTML');
  await page.getByLabel('Assignee', { exact: true }).fill('support-oncall');
  await page.getByLabel('Expected behavior (text reference)', { exact: true }).fill('Ask for approval');
  await page.getByRole('button', { name: 'Submit for review', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('#main')?.textContent.includes('in review'));
  await page.getByLabel('Review note', { exact: true }).fill('Confirmed expected behavior and data handling');
  await page.getByRole('button', { name: 'Approve this revision', exact: true }).click();
  const exported = page.getByRole('button', { name: 'Download approved case (JSONL)', exact: true });
  await exported.waitFor();
  await checkAccessibility(page);
  const downloadPromise = page.waitForEvent('download');
  await exported.click();
  const download = await downloadPromise;
  const stream = await download.createReadStream();
  const chunks = [];
  for await (const chunk of stream) chunks.push(chunk);
  const row = JSON.parse(Buffer.concat(chunks).toString());
  assert.equal(row.reference.text, 'Ask for approval');
  assert.equal(row.metadata['evalsi.failure'].revision, '3');
  assert.equal(row.metadata['evalsi.failure'].trace_id, traceId);
  const caseURL = page.url();
  await page.getByText('Publish this case to a shared dataset', { exact: true }).click();
  await page.getByLabel('Dataset name', { exact: true }).fill('incident-regressions');
  await page.getByRole('button', { name: 'Publish immutable draft', exact: true }).click();
  await page.getByRole('heading', { name: 'incident-regressions@v1', exact: true }).waitFor();
  await page.getByText('Inspect all record content', { exact: true }).click();
  assert.match(await page.textContent('#main'), /Ask for approval/);
  await checkAccessibility(page);
  await page.goto(caseURL);
  await page.getByRole('heading', { name: 'Edit case', exact: true }).waitFor();
  assert.equal(await page.getByLabel('What went wrong?', { exact: true }).count(), 1, await page.textContent('#main'));
  await page.getByLabel('What went wrong?', { exact: true }).fill('New finding needs another review');
  await page.getByRole('button', { name: 'Save draft', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('#main')?.textContent.includes('New finding needs another review'));
  await page.waitForFunction(() => !document.querySelector('#main')?.textContent.includes('Download approved case'));
  assert.equal(await page.getByRole('button', { name: 'Approve this revision', exact: true }).isDisabled(), true);
  assert.deepEqual(problems, []);
  await page.close();
});


test('a shared dataset reaches a reviewed release decision and retirement in the browser', async () => {
  const url = await startServer('dataset-catalog');
  const page = await browser.newPage();
  const problems = await watch(page);
  await page.goto(`${url}/ui/#/datasets`);
  await page.getByText('Publish a new dataset version', { exact: true }).click();
  await page.getByLabel('Dataset name', { exact: true }).fill('shared-support');
  await page.getByLabel('Owner', { exact: true }).fill('support-team');
  await page.getByLabel('Dataset records (JSON file)', { exact: true }).setInputFiles({
    name: 'cases.json', mimeType: 'application/json',
    buffer: Buffer.from(JSON.stringify([{ id: 'refund', input: 'Request refund', output: 'Approval required', reference: 'Approval required', usage: { latency_ms: 12.5 }, trajectory: { steps: [{ type: 'tool', input: 'approval', output: 'granted', usage: { latency_ms: 2 } }] } }, { id: 'handoff', input: 'Ask for help', output: 'Hand over', reference: 'Hand over' }])),
  });
  await page.getByRole('button', { name: 'Publish immutable draft', exact: true }).click();
  await page.getByRole('heading', { name: 'shared-support@v1', exact: true }).waitFor();
  const runButton = page.getByRole('button', { name: 'Start pinned regression run', exact: true });
  assert.equal(await runButton.isDisabled(), true, 'draft cannot run');
  await page.getByText('Inspect all record content', { exact: true }).click();
  assert.match(await page.textContent('#main'), /Approval required/);
  assert.match(await page.textContent('#main'), /STEP_TYPE_TOOL/);
  assert.match(await page.textContent('#main'), /0\.012500s/);
  await checkAccessibility(page);
  await page.getByLabel('Dataset review note', { exact: true }).fill('Reviewed all records in this digest');
  await page.getByRole('button', { name: 'Approve this dataset version', exact: true }).click();
  await runButton.waitFor({ state: 'visible' });
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(b => b.textContent === 'Start pinned regression run' && !b.disabled));
  await page.getByLabel('Declared agent commit (for release comparisons)', { exact: true }).fill('a'.repeat(40));
  await runButton.click();
  await page.waitForURL(/#\/runs\//);
  await page.waitForFunction(() => /succeeded/i.test(document.querySelector('#main')?.textContent ?? ''));
  await checkAccessibility(page);
  const baselineID = new URL(page.url()).hash.split('/').at(-1);
  await page.goto(`${url}/ui/#/datasets/shared-support/v1`);
  await page.getByLabel('Declared agent commit (for release comparisons)', { exact: true }).fill('b'.repeat(40));
  await page.getByRole('button', { name: 'Start pinned regression run', exact: true }).click();
  await page.waitForURL(/#\/runs\//);
  await page.waitForFunction(() => /succeeded/i.test(document.querySelector('h1')?.textContent ?? ''));
  const candidateID = new URL(page.url()).hash.split('/').at(-1);
  await page.goto(`${url}/ui/#/releases`);
  await page.getByLabel('Release decision ID', { exact: true }).fill('verified-release');
  await page.getByLabel('Baseline run ID', { exact: true }).fill(baselineID);
  await page.getByLabel('Candidate run ID', { exact: true }).fill(candidateID);
  await page.getByRole('button', { name: 'Inspect both run identities', exact: true }).click();
  await page.getByRole('heading', { name: 'Baseline identity', exact: true }).waitFor();
  await page.getByRole('button', { name: 'Inspect release policy', exact: true }).click();
  await page.getByText('Policy SHA-256:', { exact: false }).waitFor();
  await checkAccessibility(page);
  const confirm = page.getByLabel('I reviewed this policy and both run identities', { exact: true });
  await confirm.check();
  const policyInput = page.getByLabel('Release policy (JSON)', { exact: true });
  await policyInput.fill((await policyInput.inputValue()) + '\n');
  assert.equal(await confirm.isChecked(), false, 'editing policy revokes review');
  await page.getByRole('button', { name: 'Inspect release policy', exact: true }).click();
  await page.getByText('Policy SHA-256:', { exact: false }).waitFor();
  await confirm.check();
  await page.getByRole('button', { name: 'Evaluate and save release decision', exact: true }).click();
  await page.getByRole('heading', { name: 'verified-release: pass', exact: true }).waitFor();
  await checkAccessibility(page);
  const releaseDownload = page.waitForEvent('download');
  await page.getByRole('button', { name: 'Download portable release report', exact: true }).click();
  const reportStream = await (await releaseDownload).createReadStream();
  const reportChunks = [];
  for await (const chunk of reportStream) reportChunks.push(chunk);
  const report = JSON.parse(Buffer.concat(reportChunks).toString());
  assert.equal(report.schema, 'evalsi.release-report/v1');
  assert.equal(report.decision.outcome, 'RELEASE_OUTCOME_PASS');
  assert.equal(report.decision.baseline.reference.runId, baselineID);
  assert.equal(report.decision.candidate.reference.runId, candidateID);
  await page.goto(`${url}/ui/#/datasets/shared-support/v1`);
  await page.getByLabel('Dataset review note', { exact: true }).fill('Retire suite after verification');
  await page.getByRole('button', { name: 'Retire this dataset version', exact: true }).click();
  await page.waitForFunction(() => /Retire suite after verification/.test(document.querySelector('#main')?.textContent ?? ''));
  assert.equal(await runButton.isDisabled(), true, 'retired cannot run');
  assert.equal(await page.getByRole('button', { name: 'Approve this dataset version', exact: true }).isDisabled(), true);
  await checkAccessibility(page);
  await page.goto(`${url}/ui/#/releases/verified-release`);
  await page.getByRole('heading', { name: 'verified-release: pass', exact: true }).waitFor();
  await checkAccessibility(page);
  assert.deepEqual(problems, []);
  await page.close();
});

test('active run polling preserves keyboard focus and record inspection state', async () => {
  const url = await startServer('run-inspection');
  const page = await browser.newPage();
  let polls = 0;
  await page.route('**/v1alpha1/runs/inspection', route => {
    polls += 1;
    return route.fulfill({ json: { run: { id: 'inspection', name: 'Inspect active results', status: polls < 3 ? 'RUN_STATUS_RUNNING' : 'RUN_STATUS_SUCCEEDED', progress: { done: polls, total: 3 } } } });
  });
  await page.route('**/v1alpha1/runs/inspection/results?*', route => route.fulfill({ json: {
    records: [{ id: 'case', input: { text: 'Inspect this record' }, output: { text: 'answer' } }],
    results: [{ recordId: 'case', evaluator: 'exact-match', outcome: 'OUTCOME_SCORED', scores: [{ number: 1 }] }],
  } }));
  await page.goto(`${url}/ui/#/runs/inspection`);
  await page.locator('details > summary').click();
  const sort = page.getByLabel('sort by metric', { exact: true });
  await sort.selectOption('exact-match');
  // Sorting is a user action that reconstructs rows; expand the chosen row again.
  await page.locator('details > summary').click();
  await sort.focus();
  await page.waitForFunction(() => document.querySelector('.kv')?.textContent.includes('2/3'));
  assert.equal(await page.locator('details[open]').count(), 1);
  assert.equal(await sort.inputValue(), 'exact-match');
  assert.equal(await sort.evaluate(node => node === document.activeElement), true);
  await page.waitForFunction(() => /succeeded/.test(document.querySelector('h1')?.textContent ?? ''));
  await page.waitForFunction(() => document.querySelector('select[aria-label="sort by metric"]')?.value === 'exact-match');
  assert.equal(await page.locator('details[open]').count(), 1);
  assert.equal(await sort.evaluate(node => node === document.activeElement), true);
  await page.close();
});
