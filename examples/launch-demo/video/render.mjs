// Render an edited walkthrough from actual captured CLI results and HTML reports.
// FFmpeg's local flite voice supplies explicitly disclosed synthetic narration.
import { chromium } from 'playwright-core';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { readFileSync, writeFileSync, mkdirSync, copyFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const build = path.join(root, 'video/build');
const assets = path.join(root, 'assets');
mkdirSync(build, { recursive: true });
mkdirSync(assets, { recursive: true });
const scenes = JSON.parse(readFileSync(path.join(root, 'video/scenes.json'), 'utf8'));
const transcript = JSON.parse(readFileSync(path.join(root, 'results/transcript.json'), 'utf8'));
const baseline = JSON.parse(readFileSync(path.join(root, 'results/baseline.json'), 'utf8'));
const regressed = JSON.parse(readFileSync(path.join(root, 'results/regressed.json'), 'utf8'));
if (baseline.summaries[0].mean !== 1 || regressed.summaries[0].mean !== 0.5) throw Error('Unexpected controls');
if (transcript.find(x => x.command.includes('regressed.yaml'))?.exit_code !== 3) throw Error('Missing failing control');

const esc = value => String(value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const text = file => readFileSync(path.join(root, file), 'utf8');
const stamp = (seconds, separator = '.') => {
  const ms = Math.round(seconds * 1000);
  return `${String(Math.floor(ms/3600000)).padStart(2,'0')}:${String(Math.floor(ms/60000)%60).padStart(2,'0')}:${String(Math.floor(ms/1000)%60).padStart(2,'0')}${separator}${String(ms%1000).padStart(3,'0')}`;
};
const ffmpeg = args => execFileSync('ffmpeg', ['-hide_banner', '-loglevel', 'error', '-y', ...args], { stdio: ['ignore','pipe','pipe'] });
const commandPanel = (fixture, color) => {
  const row = transcript.find(x => x.command[1] === 'run' && x.command.includes(fixture));
  const display = row.command.map(x => x.replace(root + '/', '')).join(' ');
  return `<div class="terminal"><div class="termbar"><i></i><i></i><i></i><span>captured CLI output · Evals.si 0.3.1</span></div><pre><span class="prompt">$ ${esc(display)}</span>\n\n${esc(row.stdout)}</pre><div class="exit ${color}">exit code: ${row.exit_code}</div></div>`;
};

const browser = await chromium.launch({ executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium', headless: true });
const server = createServer((req,res) => {
  if(req.url !== '/report.html') { res.writeHead(404); res.end(); return; }
  res.writeHead(200, {'Content-Type':'text/html; charset=utf-8'});
  res.end(text('results/regressed.html'));
});
await new Promise(resolve => server.listen(0,'127.0.0.1',resolve));
try {
  const reportPage = await browser.newPage({viewport:{width:960,height:780}, deviceScaleFactor:1});
  await reportPage.goto(`http://127.0.0.1:${server.address().port}/report.html`);
  await reportPage.locator('summary').first().click();
  await reportPage.screenshot({path:path.join(build,'actual-report.png')});
  await reportPage.close();
  const image = readFileSync(path.join(build,'actual-report.png')).toString('base64');
  const page = await browser.newPage({viewport:{width:1280,height:720}, deviceScaleFactor:1});
  const style = `
    *{box-sizing:border-box}body{margin:0;background:#0c1725;color:#edf4fb;font:24px Arial,sans-serif}
    .slide{height:720px;padding:38px 52px 0;background:radial-gradient(ellipse at 100% 0,#173d4d,transparent 60%),#0c1725}
    header{display:flex;justify-content:space-between;align-items:center}.brand{font-weight:800;font-size:23px;letter-spacing:-1px}.brand b{color:#69e4bd}
    .step{font-size:14px;color:#9cb2c6;letter-spacing:2px;font-weight:700}h1{font-size:42px;letter-spacing:-1.4px;margin:24px 0 20px;line-height:1.1}
    main{height:430px;overflow:hidden}.hero h2{font-size:72px;line-height:1.03;max-width:900px;margin:25px 0}.hero p{font-size:25px;color:#b8cbdc;max-width:970px;line-height:1.4}
    .pills{display:flex;gap:14px;margin-top:27px}.pill{padding:12px 18px;border:1px solid #3b596c;border-radius:30px;color:#98efd2;font-size:18px}.next h2{font-size:64px;margin:16px 0}
    pre{font:20px/1.55 'DejaVu Sans Mono',monospace;white-space:pre-wrap;margin:0}.terminal{background:#08111c;border:1px solid #355367;border-radius:15px;overflow:hidden;position:relative;height:414px}.terminal pre{padding:20px 23px;font-size:18px;line-height:1.43}.prompt{color:#83e8c3}.termbar{display:flex;gap:7px;align-items:center;padding:11px 17px;background:#152738;font-size:13px;color:#abc1d2}.termbar i{width:9px;height:9px;background:#55758d;border-radius:100%}.termbar span{margin-left:12px}.exit{position:absolute;bottom:17px;right:20px;border-radius:8px;padding:9px 15px;font:700 20px 'DejaVu Sans Mono',monospace}.green{background:#163d35;color:#92f2c7}.red{background:#49272f;color:#ffb4b4}
    .card{background:#122738;border:1px solid #335267;border-radius:16px;padding:27px}.code{background:#07131e;padding:27px;border-radius:12px;color:#b5ecd9}.small{font-size:19px;color:#b9cddd;line-height:1.6}.large{font-size:30px;color:#e3f6ef}.grid{display:grid;grid-template-columns:1fr 1fr;gap:22px}.grid .code{font-size:18px;line-height:1.6}.points{display:grid;gap:16px}.point{padding:20px 24px;border-left:4px solid #69e4bd;background:#142b3c;font-size:25px;line-height:1.3}.point small{display:block;color:#b6cbdc;font-size:19px;margin-top:8px}
    table{border-collapse:collapse;width:100%;background:#102333;border-radius:12px;overflow:hidden}th,td{text-align:left;padding:24px;border-bottom:1px solid #365064;font-size:27px}th{font-size:16px;color:#9bb4c7;text-transform:uppercase;letter-spacing:1px}.bad{color:#ffb8b8}.good{color:#88e8bd}.note{font-size:19px;color:#bed0df;margin-top:22px}.report{display:grid;grid-template-columns:650px 1fr;gap:30px;height:425px}.report img{width:650px;height:425px;object-fit:contain;object-position:top;background:white;border-radius:12px}.report .card{padding:22px}.report p{font-size:22px;line-height:1.4}
    footer{position:absolute;left:52px;right:52px;bottom:20px;border-top:1px solid #365064;padding-top:13px}.caption{font-size:19px;line-height:1.4;max-width:1160px;color:#e3edf5;min-height:80px}.disclosure{position:absolute;bottom:8px;right:53px;font-size:11px;color:#92acbf}.progress{height:3px;background:#69e4bd;position:absolute;top:0;left:0}
  `;
  let cursor = 0;
  const srt = [], vtt = ['WEBVTT',''];
  const script = ['# Demo storyboard and narration','','The film is an edited walkthrough of actual captured CLI output and a screenshot of the actual generated HTML report. Timing is editorial, not a performance measurement. Voice: FFmpeg/libflite synthetic narration. The fixtures are synthetic recorded outputs, with no live agent or model calls.',''];
  for (const [i, scene] of scenes.entries()) {
    const prefix = path.join(build, `${String(i+1).padStart(2,'0')}-${scene.id}`);
    writeFileSync(prefix+'.txt',scene.narration);
    ffmpeg(['-f','lavfi','-i',`flite=textfile=${prefix}.txt:voice=slt`,'-ar','48000','-ac','1',prefix+'.wav']);
    const speech = Number(execFileSync('ffprobe',['-v','error','-show_entries','format=duration','-of','default=noprint_wrappers=1:nokey=1',prefix+'.wav'],{encoding:'utf8'}).trim());
    const duration = Math.max(10, Math.ceil((speech+1.5)*25)/25);
    let content='';
    if(scene.kind==='intro') content='<div class="hero"><h2>A passing run.<br>A failing control.<br>A report you can inspect.</h2><div class="pills"><span class="pill">Published 0.3.1</span><span class="pill">Recorded outputs</span><span class="pill">No model calls</span></div></div>';
    if(scene.kind==='install') content='<div class="card"><pre class="code">python3 -m venv .venv\nsource .venv/bin/activate\npython -m pip install \'evalsi==0.3.1\' \'evalsi-harness==0.3.1\'\nevalsi init evals-demo\ncd evals-demo</pre><p class="small">Fresh published-package installation was verified. Internet is needed for installation;<br>these fixture evaluations then run offline.</p></div>';
    if(scene.kind==='fixtures') content=`<table><thead><tr><th>Case</th><th>Baseline</th><th>Negative control</th><th>Reference</th></tr></thead><tbody>${regressed.records.map(r=>`<tr><td>${esc(r.id)}</td><td class="good">${esc(baseline.records.find(b=>b.id===r.id).output.text)}</td><td class="${r.output.text===r.reference.text?'good':'bad'}">${esc(r.output.text)}</td><td>${esc(r.reference.text)}</td></tr>`).join('')}</tbody></table><p class="note">Two synthetic cases · prewritten outputs · no live generation</p><div class="card small">The gate requires exact-match mean ≥ 1.0 and evaluator error rate ≤ 0.0.</div>`;
    if(scene.kind==='baseline') content=commandPanel('run.yaml','green');
    if(scene.kind==='regressed') content=commandPanel('regressed.yaml','red');
    if(scene.kind==='report') content=`<div class="report"><img src="data:image/png;base64,${image}" alt="Actual generated regressed HTML report"><div class="card"><pre style="font-size:17px">evalsi report\n  results/regressed.json\n  -o results/regressed.html</pre><p><b class="bad">japan: fail</b></p><p>Output: <b>Kyoto</b><br>Reference: <b>Tokyo</b></p><p class="small">Keep the raw JSON and evaluator/data identities with the report.</p></div></div>`;
    if(scene.kind==='ci') content='<div class="grid"><div class="card"><p class="large">A failed gate fails the step</p><pre class="code">- name: Evaluate\n  run: |\n    evalsi run -f evals/run.yaml \\\n      --output results.json</pre><p class="small">No continue-on-error on the gate.</p></div><div class="card"><p class="large">Preserve the evidence</p><pre class="code">- name: Report\n  if: always()\n  run: |\n    evalsi report results.json \\\n      -o report.html</pre><p class="small">The full template also uploads artifacts.</p></div></div><p class="note">Recipe shown; equivalent commands verified locally. No hosted CI execution is claimed.</p>';
    if(scene.kind==='limits') content='<div class="points"><div class="point">Two cases give wide uncertainty<small>Baseline 95% CI: [0.342, 1.000] · negative control: [0.095, 0.905]</small></div><div class="point">Static fixtures do not evaluate a changed agent<small>Generate outputs from your application before relying on the check.</small></div><div class="point">Choose an evaluator for the actual behavior<small>Review references, coverage, skips, critical cases and task-specific checks.</small></div></div>';
    if(scene.kind==='next') content='<div class="hero next"><h2>Run it.<br>Break it.<br>Inspect the evidence.</h2><p>Video + two tutorials + report + raw outputs + scripts</p><div class="card"><pre>github.com/Abhishek-Rnjn/Evals.si\nexamples/launch-demo</pre></div></div>';
    await page.setContent(`<!doctype html><html lang="en"><head><meta charset="utf-8"><style>${style}</style></head><body><div class="progress" style="width:${(i+1)/scenes.length*100}%"></div><div class="slide"><header><div class="brand">evals<b>.si</b></div><div class="step">${esc(scene.eyebrow)}</div></header><h1>${esc(scene.title)}</h1><main>${content}</main><footer><div class="caption">${esc(scene.narration)}</div></footer><div class="disclosure">Synthetic recorded fixtures · synthetic voice · actual CLI results</div></div></body></html>`);
    await page.screenshot({path:prefix+'.png'});
    if(i===0) copyFileSync(prefix+'.png',path.join(assets,'poster.png'));
    ffmpeg(['-loop','1','-framerate','25','-i',prefix+'.png','-i',prefix+'.wav','-t',String(duration),'-vf','format=yuv420p','-af','apad','-c:v','libx264','-preset','fast','-crf','21','-c:a','aac','-b:a','128k','-movflags','+faststart',prefix+'.mp4']);
    srt.push(`${i+1}\n${stamp(cursor,',')} --> ${stamp(cursor+duration,',')}\n${scene.narration}\n`);
    vtt.push(`${stamp(cursor)} --> ${stamp(cursor+duration)}\n${scene.narration}\n`);
    script.push(`## ${stamp(cursor)} — ${scene.title}`,`Visual: ${scene.kind}. Duration: ${duration.toFixed(2)} seconds.`, '',scene.narration,'');
    scene.duration=duration;scene.start=cursor;scene.end=cursor+duration;
    cursor+=duration;
    console.log(`Rendered ${scene.id} (${duration.toFixed(2)} s)`);
  }
  writeFileSync(path.join(assets,'demo.srt'),srt.join('\n'));
  writeFileSync(path.join(assets,'demo.vtt'),vtt.join('\n'));
  writeFileSync(path.join(root,'video/SCRIPT.md'),script.join('\n'));
  writeFileSync(path.join(build,'concat.txt'),scenes.map((s,i)=>`file '${String(i+1).padStart(2,'0')}-${s.id}.mp4'`).join('\n'));
  ffmpeg(['-f','concat','-safe','0','-i',path.join(build,'concat.txt'),'-c','copy',path.join(build,'combined.mp4')]);
  ffmpeg(['-i',path.join(build,'combined.mp4'),'-i',path.join(assets,'demo.srt'),'-c:v','copy','-c:a','copy','-c:s','mov_text','-metadata:s:s:0','language=eng','-disposition:s:0','default','-metadata','title=Evals.si: catch a recorded-output regression','-metadata','comment=Edited walkthrough of actual CLI results; synthetic fixtures and synthetic narration.','-movflags','+faststart',path.join(assets,'demo.mp4')]);
  const digest = file => createHash('sha256').update(readFileSync(path.join(root,file))).digest('hex');
  writeFileSync(path.join(assets,'production.json'),JSON.stringify({width:1280,height:720,fps:25,duration_seconds:cursor,voice:'FFmpeg libflite slt (synthetic)',type:'edited walkthrough using actual captured CLI output and an actual generated report screenshot',scenes:scenes.map(({id,title,start,end})=>({id,title,start,end})),evidence_sha256:Object.fromEntries(['results/baseline.json','results/regressed.json','results/transcript.json','results/regressed.html'].map(file=>[file,digest(file)])),video_sha256:digest('assets/demo.mp4')},null,2)+'\n');
  console.log(`Finished: ${cursor.toFixed(2)} seconds; assets/demo.mp4`);
} finally { await browser.close(); server.close(); }
