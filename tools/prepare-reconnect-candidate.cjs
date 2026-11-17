'use strict';
// Temporary, exact-source candidate preparation. Removed after verification.
const fs=require('node:fs');
const path=require('node:path');
const crypto=require('node:crypto');
const root=process.argv[2]||'.';
const htmlPath=path.join(root,'internal/webui/access.html');
let html=fs.readFileSync(htmlPath,'utf8').replace(/\r\n/g,'\n');
const before=crypto.createHash('sha256').update(html).digest('hex');
if(before!=='1fef2d25c7ede4e1118491be0f016e14fd4206f91cb4faa0e3e707effc19a7b6')throw new Error('Unexpected baseline UI');
function replaceOnce(before,after){
 if(html.split(before).length!==2)throw new Error('Unexpected UI source: '+before.slice(0,100));
 html=html.replace(before,after);
}
replaceOnce(" // Background status refreshes must preserve the log viewport in place.\n const existingLog=preserveLog?$('#root .log-panel'):null;\n const existingLogScroll=existingLog?.querySelector('#liveLog')?.scrollTop??0;",
" // Preserve only the log viewport. The header must reflect current feedback.\n const existingLog=preserveLog?$('#root .log-panel'):null;\n const existingLogBody=existingLog?.querySelector('#liveLog');\n const existingLogScroll=existingLogBody?.scrollTop??0;");
replaceOnce("   replacement.replaceWith(existingLog);\n   const scroller=existingLog.querySelector('#liveLog');\n   if(scroller)scroller.scrollTop=existingLogScroll;",
"   const replacementBody=replacement.querySelector('#liveLog');\n   if(existingLogBody&&replacementBody){\n    replacementBody.replaceWith(existingLogBody);\n    existingLogBody.scrollTop=existingLogScroll;\n   }");
replaceOnce(" // The UI must not block reconnection. Defer rendering while the user edits.\n if(!app.ready||refreshing||document.hidden||\n    (!reenter&&!connection.offline&&state.route!=='/processes')||\n    (!connection.offline&&editing))return;",
" // Editing defers rendering, never the connection probe. A paused log must\n // not hide a dead connection merely because an input or dialog has focus.\n if(!app.ready||refreshing||document.hidden||\n    (!reenter&&!connection.offline&&state.route!=='/processes'))return;");
replaceOnce(" connection.recovering=reenter||connection.offline;\n try{\n  if(reenter||connection.offline){",
" connection.recovering=reenter||connection.offline;\n try{\n  if(editing&&!connection.recovering){\n   // Probe without replacing the editor's data/revision or optimistic locks.\n   await request('/api/access/state');\n   return;\n  }\n  if(reenter||connection.offline){");
for(const match of html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g))new Function(match[1]);
fs.writeFileSync(htmlPath,html);
const testPath=path.join(root,'tools/test-reconnect-browser.cjs');
let test=fs.readFileSync(testPath,'utf8').replace(/\r\n/g,'\n');
const oldPause="   await page.evaluate(()=>{logState.paused=true;});\n   await page.locator('[data-act=\"new-process\"]').click();await page.locator('#processName').fill('unsaved reconnect draft');\n   mode='unavailable';await offline();recoverProxy();await recovered();";
const newPause="   await page.locator('[data-act=\"new-process\"]').click();await page.locator('#processName').fill('unsaved reconnect draft');\n   await page.waitForFunction(()=>!logBusy&&!refreshing);\n   await page.evaluate(async()=>{await pollLog(true);logState.paused=true;});\n   assert.equal(await page.evaluate(()=>logState.paused&&logState.id===state.procSelected),true);\n   mode='unavailable';await offline();recoverProxy();await recovered();";
if(!test.includes(oldPause))throw new Error('Unexpected pause test');test=test.replace(oldPause,newPause);
const oldRestart="   const readsBefore=logReads;\n   await stop();await offline();await start();await recovered();\n   assert.equal(await page.evaluate(()=>window.__reconnectPageToken),token,'must not reload the page');\n   await page.waitForTimeout(2000);assert.ok(logReads>readsBefore,'log polling resumes');";
const newRestart="   for(let cycle=0;cycle<3;cycle++){\n    const readsBefore=logReads;\n    await stop();await offline();await start();await recovered();\n    assert.equal(await page.evaluate(()=>window.__reconnectPageToken),token,'must not reload the page');\n    await page.waitForTimeout(2000);assert.ok(logReads>readsBefore,'log polling resumes');\n   }";
if(!test.includes(oldRestart))throw new Error('Unexpected restart test');test=test.replace(oldRestart,newRestart);
const extra=`  await scenario('browser network offline/online recovers without reload',async()=>{
   const token=await page.evaluate(()=>{window.__networkPageToken=Math.random();return window.__networkPageToken;});
   await page.context().setOffline(true);
   try{await offline();}finally{await page.context().setOffline(false);}
   await recovered();
   assert.equal(await page.evaluate(()=>window.__networkPageToken),token);
   assert.equal(await page.locator('[data-act="run"]').first().isEnabled(),true);
  });
  await scenario('healthy editor probe preserves its database revision and unsaved input',async()=>{
   await page.locator('[data-act="new-process"]').click();await page.locator('#processName').fill('stable draft');
   await page.waitForFunction(()=>!refreshing&&!logBusy);
   const snapshot=await page.evaluate(()=>{window.__editorDB=db;return {revision:app.revision,actor:state.actor};});
   await page.evaluate(()=>refreshBackground());
   assert.equal(await page.evaluate(()=>db===window.__editorDB),true);
   assert.equal(await page.evaluate(()=>app.revision),snapshot.revision);
   assert.equal(await page.locator('#processName').inputValue(),'stable draft');
   await page.evaluate(()=>closeModal(true));
  });
`;
const finalAssert="  assert.deepEqual(report.pageErrors,[],'uncaught browser errors');";
if(!test.includes(finalAssert))throw new Error('Unexpected final assertion');
test=test.replace(finalAssert,extra+finalAssert);new Function('require','__dirname',test);fs.writeFileSync(testPath,test);
console.log(JSON.stringify({before,after:crypto.createHash('sha256').update(html).digest('hex')}));
