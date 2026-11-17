'use strict';
// Exercise the shipped UI in Chromium against a real, disposable SpawnPoint.
// The proxy injects network faults; no production service or database is used.
const {chromium}=require('playwright');
const assert=require('node:assert/strict');
const {spawn}=require('node:child_process');
const crypto=require('node:crypto');
const fs=require('node:fs');
const http=require('node:http');
const os=require('node:os');
const path=require('node:path');
const sleep=ms=>new Promise(resolve=>setTimeout(resolve,ms));
const listen=server=>new Promise((resolve,reject)=>{server.once('error',reject);server.listen(0,'127.0.0.1',()=>resolve(server.address().port));});
(async()=>{
 const exe=path.resolve(process.argv[2]);
 assert.ok(fs.existsSync(exe),'pass the built SpawnPoint executable');
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),'spawnpoint-reconnect-'));
 fs.mkdirSync('artifacts',{recursive:true});
 // Record the exact static source and executable fingerprint being exercised.
 fs.copyFileSync('internal/webui/access.html','artifacts/reconnect-source.html');
 const report={platform:process.platform,exeSHA256:crypto.createHash('sha256').update(fs.readFileSync(exe)).digest('hex'),scenarios:[],pageErrors:[]};
 const reserve=http.createServer();const backendPort=await listen(reserve);await new Promise(resolve=>reserve.close(resolve));
 const setupToken=crypto.randomBytes(32).toString('hex');
 const password='Test!'+crypto.randomBytes(24).toString('hex');
 const env={...process.env,SPAWNPOINT_HOST:'127.0.0.1',SPAWNPOINT_PORT:String(backendPort),SPAWNPOINT_DB_PATH:path.join(dir,'db'),SPAWNPOINT_LOG_DIR:path.join(dir,'logs'),SPAWNPOINT_API_TOKENS:'',SPAWNPOINT_SETUP_TOKEN:setupToken};
 let server,browser,page,mode='online',commandCount=0,logReads=0;
 const held=new Set();
 async function start(){
  server=spawn(exe,[],{env,stdio:'ignore'});
  let launchError;server.once('error',error=>{launchError=error;});
  for(let i=0;i<150;i++){
   if(launchError)throw launchError;
   if(server.exitCode!==null)throw new Error('test server exited during startup');
   try{if((await fetch(`http://127.0.0.1:${backendPort}/healthz`,{signal:AbortSignal.timeout(1000)})).ok)return;}catch{}
   await sleep(100);
  }
  throw new Error('test server did not start');
 }
 async function stop(){
  if(!server||server.exitCode!==null)return;
  const child=server;
  await new Promise(resolve=>{const timer=setTimeout(()=>{child.kill('SIGKILL');resolve();},10000);child.once('exit',()=>{clearTimeout(timer);resolve();});child.kill('SIGTERM');});
 }
 const proxy=http.createServer((req,res)=>{
  const command=/\/processes\/[^/]+\/(run|stop|restart)$/.test(req.url)&&req.method==='POST';
  if(command){commandCount++;if(mode==='drop-command')mode='unavailable';}
  if(/\/processes\/[^/]+\/logs/.test(req.url))logReads++;
  const api=/^\/(auth|api|processes|runtime|help)(\/|\?|$)/.test(req.url);
  if(mode==='unavailable'&&api){res.writeHead(503,{'Content-Type':'application/json'});res.end('{"ok":false}');return;}
  if((mode==='hang-headers'||mode==='hang-body')&&req.url==='/api/access/state'){
   held.add(res);res.on('close',()=>held.delete(res));
   if(mode==='hang-body'){res.writeHead(200,{'Content-Type':'application/json'});res.write('{"ok":true,');}
   return;
  }
  const upstream=http.request({hostname:'127.0.0.1',port:backendPort,path:req.url,method:req.method,headers:req.headers},incoming=>{res.writeHead(incoming.statusCode,incoming.headers);incoming.pipe(res);});
  upstream.on('error',()=>{if(!res.headersSent)res.writeHead(503,{'Content-Type':'application/json'});res.end('{"ok":false}');});
  res.on('close',()=>upstream.destroy());req.pipe(upstream);
 });
 const port=await listen(proxy),origin=`http://127.0.0.1:${port}`;
 const recoverProxy=()=>{mode='online';for(const res of held)res.destroy();held.clear();};
 async function offline(){await page.waitForFunction(()=>connection.offline,{},{timeout:18000});}
 async function recovered(){await page.waitForFunction(()=>!connection.offline&&!refreshing&&state.actor!==null,{},{timeout:30000});}
 async function fresh(){
  recoverProxy();await page.goto(origin);await page.waitForFunction(()=>app.ready&&state.route==='/processes');
  if(await page.locator('[data-process-row]').count())await page.locator('[data-process-row]').first().click();
 }
 async function scenario(name,fn){
  try{await fresh();await fn();report.scenarios.push({name,ok:true});console.log('PASS:',name);}
  catch(error){report.scenarios.push({name,ok:false,error:String(error)});console.error('FAIL:',name,error);await page.screenshot({path:`artifacts/reconnect-${report.scenarios.length}.png`,fullPage:true}).catch(()=>{});}
  finally{recoverProxy();}
 }
 try{
  await start();
  const staticHTML=await(await fetch(origin)).text();fs.writeFileSync('artifacts/reconnect-served.html',staticHTML);
  assert.equal(staticHTML,fs.readFileSync('internal/webui/access.html','utf8'),'built executable must embed the current UI');
  browser=await chromium.launch({headless:true});page=await browser.newPage({viewport:{width:1440,height:1000}});
  page.on('pageerror',error=>report.pageErrors.push(String(error)));
  const setup=await page.request.post(origin+'/auth/setup',{data:{login:'reconnect-admin',name:'Reconnect test',password,setup_token:setupToken,remember:true}});
  assert.equal(setup.status(),200,'isolated administrator setup');
  const login=await page.request.post(origin+'/auth/login',{data:{method:'local',login:'reconnect-admin',password,remember:true}});
  assert.equal(login.status(),200,'remembered test session');
  await fresh();
  await page.locator('[data-act="new-process"]').click();
  await page.locator('#processName').fill('Reconnect probe');
  await page.locator('#processCommand').fill('echo reconnect-probe');
  await page.locator('#modalForm button[name="mode"][value="save"]').click();
  await page.waitForFunction(()=>!document.getElementById('modal').open&&db.processes.length===1);
  await scenario('lost command recovers state, controls and log header without replay',async()=>{
   const before=commandCount;mode='drop-command';
   await page.locator('[data-act="run"]').first().click();
   await offline();await page.waitForFunction(()=>[...processFeedback.values()].some(value=>value.status==='unknown'));
   assert.equal(await page.locator('[data-act="retry-connection"]').count(),1,'offline retry must be visible');
   recoverProxy();await recovered();
   assert.equal(commandCount,before+1,'a lost command must never be replayed');
   assert.equal(await page.locator('.process-list-panel .process-action-feedback').count(),0,'table status must reconcile');
   assert.equal(await page.locator('.log-panel .process-action-feedback').count(),0,'log header must reconcile too');
   assert.equal(await page.locator('[data-act="run"]').first().isEnabled(),true);
  });
  await scenario('open editor does not suppress disconnection detection when logs are paused',async()=>{
   await page.evaluate(()=>{logState.paused=true;});
   await page.locator('[data-act="new-process"]').click();await page.locator('#processName').fill('unsaved reconnect draft');
   mode='unavailable';await offline();recoverProxy();await recovered();
   assert.equal(await page.locator('#processName').inputValue(),'unsaved reconnect draft');
   assert.equal(await page.locator('#modal').evaluate(node=>node.open),true);
   await page.evaluate(()=>closeModal(true));
  });
  for(const fault of ['hang-headers','hang-body'])await scenario(fault+' releases the refresh lock and resumes polling',async()=>{
   mode=fault;await offline();recoverProxy();await recovered();
   assert.equal(await page.locator('[data-act="retry-connection"]').count(),0);
  });
  await scenario('real server stop/start keeps the same browser page and remembered login',async()=>{
   const token=await page.evaluate(()=>{window.__reconnectPageToken=Math.random();return window.__reconnectPageToken;});
   const readsBefore=logReads;
   await stop();await offline();await start();await recovered();
   assert.equal(await page.evaluate(()=>window.__reconnectPageToken),token,'must not reload the page');
   await page.waitForTimeout(2000);assert.ok(logReads>readsBefore,'log polling resumes');
   assert.equal(await page.locator('[data-act="new-process"]').count(),1,'remembered login survives restart');
  });
  assert.deepEqual(report.pageErrors,[],'uncaught browser errors');
 }catch(error){report.fatal=String(error);console.error(error);}
 finally{
  recoverProxy();fs.writeFileSync('artifacts/reconnect-report.json',JSON.stringify(report,null,2));
  if(page)await page.screenshot({path:'artifacts/reconnect-final.png',fullPage:true}).catch(()=>{});
  if(browser)await browser.close();await stop();
  proxy.closeAllConnections();await new Promise(resolve=>proxy.close(resolve));
  fs.rmSync(dir,{recursive:true,force:true});
 }
 if(report.fatal||report.pageErrors.length||report.scenarios.some(result=>!result.ok))process.exitCode=1;
})().catch(error=>{console.error(error);process.exitCode=1;});
