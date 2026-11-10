// Records docs/img/demo.gif, the terminal demo at the top of the README.
//
// Nothing in it is typed by hand: the script installs the published package
// into a new virtual environment, runs each command below in a real shell and
// draws what the command printed. Where output is cut, it is cut, never edited:
// some steps draw only their first or last lines, and a line wider than the
// window ends in an ellipsis.
//
//   node scripts/make-demo-gif.mjs            # the latest published evalsi
//   EVALSI_DEMO_PACKAGE=./python/evalsi node scripts/make-demo-gif.mjs
//
// Needs python3, ffmpeg and the browser test's playwright-core (`npm ci` in
// tests/ui); CHROMIUM_PATH names a Chromium when playwright-core has none.
import { execFileSync, spawnSync } from 'node:child_process';
import { existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const { chromium } = createRequire(path.join(root, 'tests/ui/package.json'))('playwright-core');
const out = path.join(root, 'docs/img/demo.gif');
const pkg = process.env.EVALSI_DEMO_PACKAGE ?? 'evalsi';

// Each step: the command as shown, and how many of its last (`keep`) or first
// (`first`) output lines to draw; without either, all of them.
const steps = [
  { command: `pip install ${pkg === 'evalsi' ? 'evalsi' : pkg}`, show: 'pip install evalsi', keep: 1 },
  { command: 'evalsi init my-evals && cd my-evals', first: 1 },
  { command: 'evalsi run -f run.yaml', clear: true },
  { command: 'evalsi run -f regressed.yaml' },
  { command: 'echo "exit code: $?"', hold: 4.5 },
];

const ROWS = 23;
const COLUMNS = 88;
const WIDTH = 860;
const TYPE_CHARS = 2; // characters per typing frame
const TYPE_SECONDS = 0.07;
const OUTPUT_SECONDS = 1.9;

// One shell for every step, so `cd` and `$?` carry over as they would for a person.
function run(work) {
  const marker = '__EVALSI_DEMO_STEP__';
  const script = [
    'python3 -m venv venv >/dev/null && . venv/bin/activate',
    'export PIP_DISABLE_PIP_VERSION_CHECK=1 NO_COLOR=1',
    ...steps.map((s) => `${s.command} 2>&1; __code=$?; echo ${marker}; (exit $__code)`),
  ].join('\n');
  const result = spawnSync('bash', ['-c', script], { cwd: work, encoding: 'utf8' });
  const outputs = result.stdout.split(`${marker}\n`).slice(0, steps.length);
  if (outputs.length !== steps.length) throw new Error(`the demo shell stopped early:\n${result.stdout}\n${result.stderr}`);
  return outputs.map((text, i) => {
    const lines = text.replace(/\n$/, '').split('\n');
    const kept = steps[i].keep ? lines.slice(-steps[i].keep) : steps[i].first ? lines.slice(0, steps[i].first) : lines;
    // A line wider than the window is cut, and says so.
    return kept.map((line) => (line.length > COLUMNS ? `${line.slice(0, COLUMNS - 2)} …` : line));
  });
}

const escape = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
function colour(line) {
  return escape(line)
    .replace(/\[pass\]/g, '<span class="ok">[pass]</span>')
    .replace(/\[FAIL\]/g, '<span class="bad">[FAIL]</span>')
    .replace(/^(exit code: )([1-9]\d*)$/, '$1<span class="bad">$2</span>');
}
const prompt = (typed, cursor) => `<span class="ps">$</span> ${escape(typed)}${cursor ? '<span class="cur"> </span>' : ''}`;

function page(lines) {
  const shown = lines.slice(-ROWS);
  while (shown.length < ROWS) shown.push('');
  return `<!doctype html><meta charset="utf-8"><style>
    body { margin: 0; background: #0d1117; }
    .bar { height: 34px; background: #161b22; display: flex; align-items: center; padding-left: 14px; gap: 8px; }
    .bar i { width: 12px; height: 12px; border-radius: 50%; display: block; }
    pre { margin: 0; padding: 14px 18px 16px; color: #e6edf3; font: 15px/21px "DejaVu Sans Mono", Menlo, Consolas, monospace; white-space: pre; }
    .ps { color: #7ee787; } .ok { color: #3fb950; font-weight: bold; } .bad { color: #ff7b72; font-weight: bold; }
    .cur { background: #e6edf3; }
  </style><div class="bar"><i style="background:#ff5f57"></i><i style="background:#febc2e"></i><i style="background:#28c840"></i></div><pre>${shown.join('\n')}</pre>`;
}

const work = mkdtempSync(path.join(tmpdir(), 'evalsi-demo-'));
try {
  const outputs = run(work);
  // The frames: [html, seconds].
  const frames = [];
  let screen = [];
  steps.forEach((step, i) => {
    const text = step.show ?? step.command;
    if (step.clear) screen = [];
    for (let n = 0; n <= text.length; n += TYPE_CHARS) {
      frames.push([page([...screen, prompt(text.slice(0, n), true)]), n === 0 ? 0.6 : TYPE_SECONDS]);
    }
    frames.push([page([...screen, prompt(text, true)]), 0.35]);
    screen = [...screen, prompt(text, false), ...outputs[i].map(colour), ''];
    frames.push([page([...screen, prompt('', true)]), step.hold ?? OUTPUT_SECONDS]);
  });

  const executablePath = process.env.CHROMIUM_PATH;
  const browser = await chromium.launch(executablePath && existsSync(executablePath) ? { executablePath } : {});
  const tab = await browser.newPage({ viewport: { width: WIDTH, height: 34 + 30 + ROWS * 21 }, deviceScaleFactor: 1 });
  const list = [];
  for (const [i, [html, seconds]] of frames.entries()) {
    const file = path.join(work, `frame-${String(i).padStart(4, '0')}.png`);
    await tab.setContent(html);
    await tab.screenshot({ path: file });
    list.push(`file '${file}'\nduration ${seconds}`);
  }
  await browser.close();
  // The concat demuxer drops the last duration unless the last file is named again.
  list.push(list.at(-1).split('\n')[0]);
  const concat = path.join(work, 'frames.txt');
  writeFileSync(concat, `${list.join('\n')}\n`);
  mkdirSync(path.dirname(out), { recursive: true });
  execFileSync('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'concat', '-safe', '0', '-i', concat,
    '-vf', 'split[a][b];[a]palettegen=max_colors=32:stats_mode=diff[p];[b][p]paletteuse=dither=none',
    '-loop', '0', out]);
  const seconds = frames.reduce((sum, [, s]) => sum + s, 0);
  console.log(`${path.relative(root, out)}: ${frames.length} frames, ${seconds.toFixed(1)}s`);
} finally {
  rmSync(work, { recursive: true, force: true });
}
