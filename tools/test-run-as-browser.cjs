'use strict';
const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {spawn}=require('node:child_process');
const fs=require('node:fs');const os=require('node:os');const path=require('node:path');
(async()=>{
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'spawnpoint-ui-'));
 const exe=path.resolve(process.argv[2]); const port=18529;
 const proc=spawn(exe,[],{env:{...process.env,SPAWNPOINT_HOST:'127.0.0.1',SPAWNPOINT_PORT:String(port),SPAWNPOINT_DB_PATH:path.join(dir,'db'),SPAWNPOINT_LOG_DIR:path.join(dir,'logs'),SPAWNPOINT_API_TOKENS:''},stdio:'ignore'});
 let browser;
 try{
  for(let i=0;i<100;i++){try{if((await fetch(`http://127.0.0.1:${port}/healthz`)).ok)break;}catch{}await new Promise(r=>setTimeout(r,100));}
  browser=await chromium.launch({headless:true});const page=await browser.newPage({viewport:{width:1440,height:1000}});
  const errors=[];page.on('pageerror',e=>errors.push(String(e)));
  await page.goto(`http://127.0.0.1:${port}`);
  await page.locator('#newBtn').click();await page.locator('#fLabel').fill('FlowGate-Agent');await page.locator('#fCmd').fill('printf "%s\\n" "preview test"');
  await page.locator('#runSpecific').click();await page.locator('#usernameInput').fill(process.env.SPAWNPOINT_RUNAS_TEST_USER);await page.locator('#passwordInput').fill(process.env.SPAWNPOINT_RUNAS_TEST_PASSWORD);
  assert.equal(await page.locator('#passwordInput').getAttribute('type'),'password');
  await page.locator('#dlgSave').click();await page.waitForFunction(()=>!document.getElementById('formBackdrop').classList.contains('open'));
  const list=await (await page.request.get(`http://127.0.0.1:${port}/processes`)).json();assert.equal(list.processes.length,1);assert.equal(list.processes[0].status,'stopped');assert.ok(list.processes[0].run_as.has_password);
  assert.ok(!JSON.stringify(list).includes(process.env.SPAWNPOINT_RUNAS_TEST_PASSWORD));
  await page.locator('button[data-act="edit"]').first().click();assert.equal(await page.locator('#passwordInput').inputValue(),'');
  assert.equal(await page.locator('#usernameInput').inputValue(),process.env.SPAWNPOINT_RUNAS_TEST_USER);
  fs.mkdirSync('artifacts',{recursive:true});await page.screenshot({path:'artifacts/editor-desktop.png',fullPage:true});
  for(const viewport of [{width:390,height:844},{width:2560,height:1440}]){
    await page.setViewportSize(viewport);
    const box=await page.locator('#formDlg').boundingBox();assert.ok(box.x>=-1 && box.x+box.width<=viewport.width+1,'dialog overflows viewport');
    assert.ok(await page.locator('#dlgSave').isVisible());await page.screenshot({path:`artifacts/editor-${viewport.width}.png`,fullPage:true});
  }
  assert.deepEqual(errors,[]);console.log('PASS: actual browser/API save, credential redaction, edit retain, responsive dialog and screenshots');
 }finally{if(browser)await browser.close();proc.kill('SIGTERM');await new Promise(r=>{if(proc.exitCode!==null)return r();proc.on('exit',r);setTimeout(r,10000);});fs.rmSync(dir,{recursive:true,force:true});}
})().catch(e=>{console.error(e);process.exitCode=1});
