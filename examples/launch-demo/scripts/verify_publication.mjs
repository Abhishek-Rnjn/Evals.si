// Validate the packaged page and video without publishing or contacting a service.
import { chromium } from 'playwright-core';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { createServer } from 'node:http';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const mime={'.html':'text/html','.mp4':'video/mp4','.vtt':'text/vtt','.png':'image/png','.json':'application/json','.md':'text/plain'};
const server=createServer((req,res)=>{
  const file=path.resolve(root,'.'+decodeURIComponent(new URL(req.url,'http://localhost').pathname));
  if(!file.startsWith(root+path.sep)||!existsSync(file)){res.writeHead(404);res.end();return;}
  const data=readFileSync(file);
  res.writeHead(200,{'Content-Type':mime[path.extname(file)]||'application/octet-stream','Content-Length':data.length});
  res.end(data);
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const origin=`http://127.0.0.1:${server.address().port}`;
const browser=await chromium.launch({executablePath:process.env.CHROMIUM_PATH||'/usr/bin/chromium',headless:true});
const checks=[];
try{
  const info=JSON.parse(execFileSync('ffprobe',['-v','error','-show_format','-show_streams','-of','json',path.join(root,'assets/demo.mp4')],{encoding:'utf8'}));
  const video=info.streams.find(s=>s.codec_type==='video');
  assert.equal(video.width,1280);assert.equal(video.height,720);assert.equal(video.codec_name,'h264');
  assert(info.streams.some(s=>s.codec_type==='audio'&&s.codec_name==='aac'));
  assert(info.streams.some(s=>s.codec_type==='subtitle'&&s.codec_name==='mov_text'));
  const duration=Number(info.format.duration);assert(duration>100&&duration<240);
  const production=JSON.parse(readFileSync(path.join(root,'assets/production.json'),'utf8'));
  const digest=file=>createHash('sha256').update(readFileSync(path.join(root,file))).digest('hex');
  assert.equal(digest('assets/demo.mp4'),production.video_sha256);
  for(const [file,expected] of Object.entries(production.evidence_sha256))assert.equal(digest(file),expected,file);
  checks.push({name:'video_codecs_and_evidence_hashes',passed:true,duration_seconds:duration});
  const failures=[];
  const page=await browser.newPage({viewport:{width:1280,height:900}});
  page.on('pageerror',err=>failures.push(err.message));
  await page.goto(origin+'/index.html');
  const localLinks=await page.locator('a[href],source[src],track[src]').evaluateAll(nodes=>nodes.map(n=>n.getAttribute('href')||n.getAttribute('src')).filter(s=>s&&!/^(https?:|#)/.test(s)));
  for(const relative of new Set(localLinks)){
    assert(existsSync(path.resolve(root,relative)),relative);
    const response=await page.request.get(origin+'/'+relative);assert.equal(response.status(),200,relative);
  }
  checks.push({name:'static_page_local_links',passed:true,count:new Set(localLinks).size});
  await page.locator('video').evaluate(async element=>{
    element.muted=true;
    await element.play();
  });
  await page.waitForFunction(()=>document.querySelector('video').currentTime>1);
  const playback=await page.locator('video').evaluate(v=>({width:v.videoWidth,height:v.videoHeight,error:v.error?.message||null}));
  assert.equal(playback.width,1280);assert.equal(playback.height,720);assert.equal(playback.error,null);
  await page.waitForFunction(()=>[...document.querySelector('video').textTracks].some(t=>t.cues?.length===9));
  checks.push({name:'browser_video_playback_and_nine_caption_cues',passed:true});
  await page.locator('video').evaluate(v=>v.pause());
  for(const [width,height] of [[1280,900],[390,844]]){
    await page.setViewportSize({width,height});
    assert(await page.evaluate(()=>document.documentElement.scrollWidth<=window.innerWidth));
  }
  checks.push({name:'desktop_and_mobile_no_horizontal_overflow',passed:true,widths:[1280,390]});
  await page.goto(origin+'/results/regressed.html');
  await page.locator('summary').first().click();
  const body=await page.locator('body').innerText();
  for(const term of ['japan','Kyoto','Tokyo','0.500'])assert(body.includes(term),term);
  assert.equal(failures.length,0,failures.join('\n'));
  checks.push({name:'actual_html_report_failed_case_and_no_page_errors',passed:true});
  const result={status:'passed',checks,note:'Local package/browser validation. No hosted GitHub Actions execution or website deployment is claimed.'};
  writeFileSync(path.join(root,'results/publication-validation.json'),JSON.stringify(result,null,2)+'\n');
  console.log(JSON.stringify(result,null,2));
}finally{await browser.close();server.close();}
