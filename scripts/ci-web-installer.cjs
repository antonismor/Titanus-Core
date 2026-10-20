// Browser smoke against the real embedded HTTPS service and durable state.
// Fixture plans reference documentation IPs and absent archives; no SSH or apply.
const {chromium}=require(process.env.TITANUS_PLAYWRIGHT_MODULE||'playwright');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const os=require('node:os');
const path=require('node:path');
const net=require('node:net');
const {spawn}=require('node:child_process');
const {setTimeout:delay}=require('node:timers/promises');
(async()=>{
 const directory=fs.mkdtempSync(path.join(os.tmpdir(),'titanus-web-browser-'));
 const stateDir=path.join(directory,'state');const logFile=path.join(directory,'installer.log');
 const listener=net.createServer();await new Promise(r=>listener.listen(0,'127.0.0.1',r));
 const port=listener.address().port;await new Promise(r=>listener.close(r));
 const origin=`https://127.0.0.1:${port}`;
 const binary=path.resolve(process.env.TITANUS_INSTALLER_BINARY||'bin/titanus-installer-web');
 let processHandle,browser;
 let diagnostics='';
 async function start(){
  processHandle=spawn(binary,['--listen',`127.0.0.1:${port}`,'--state-dir',stateDir,'--log-file',logFile]);
  processHandle.stderr.on('data',data=>diagnostics+=data);
  for(let i=0;i<100;i++){
   if(processHandle.exitCode!==null) throw Error(`installer exited: ${diagnostics}`);
   try{const probe=await fetch(origin,{signal:AbortSignal.timeout(200)});if(probe.ok)return;}catch(_){}
   // Probe TLS with the context below, rather than trusting the self-signed identity globally.
   if(fs.existsSync(path.join(stateDir,'access.token'))){await delay(150);return;}
   await delay(50);
  }
  throw Error(`installer did not start: ${diagnostics}`);
 }
 async function stop(){if(processHandle&&processHandle.exitCode===null){const ended=new Promise(r=>processHandle.once('exit',r));processHandle.kill('SIGTERM');await ended;}}
 try{
  await start();
  browser=await chromium.launch({headless:true,...(process.env.TITANUS_UI_CHROME?{executablePath:process.env.TITANUS_UI_CHROME}:{}),args:['--no-sandbox']});
  const context=await browser.newContext({ignoreHTTPSErrors:true,viewport:{width:1672,height:1000}});
  const page=await context.newPage();const errors=[];page.on('pageerror',error=>errors.push(String(error)));
  await page.goto(origin);await page.locator('#accessGate').waitFor({state:'visible'});
  const token=fs.readFileSync(path.join(stateDir,'access.token'),'utf8').trim();
  await page.goto(`${origin}/#access=${token}`);await page.locator('#nodeCount').waitFor();
  assert.equal(new URL(page.url()).hash,'','access fragment was retained');
  assert((await context.cookies()).some(c=>c.name==='titanus_installer_session'&&c.httpOnly&&c.secure&&c.sameSite==='Strict'));
  assert((await page.locator('body').innerText()).includes('SOON AVAILABLE'));
  await page.selectOption('#nodeCount','1');await page.fill('#realmName','BROWSER-REALM');
  await page.click('#toInfrastructure');await page.fill('#nodeMgmt0','192.0.2.10');
  await page.fill('#sshPassword','BROWSER_PASSWORD_CANARY');await page.click('#toComponents');
  await page.fill('#releaseVersion','0.4.0-rc.17');await page.fill('#revision','a'.repeat(40));
  await page.fill('#amd64Path',path.join(directory,'absent-amd64.tar.gz'));await page.fill('#amd64SHA','b'.repeat(64));
  await page.fill('#arm64Path',path.join(directory,'absent-arm64.tar.gz'));await page.fill('#arm64SHA','c'.repeat(64));
  await page.click('#toReview');await page.click('#prepareOnly');
  await page.waitForFunction(()=>document.querySelector('#progressMessage')?.textContent.includes('stopped safely'));
  const planResponse=await context.request.get(origin+'/api/v1/plan');assert.equal(planResponse.status(),200);
  const plan=await planResponse.json();assert.equal(plan.realm_name,'BROWSER-REALM');assert.equal(plan.nodes.length,1);
  for(const name of ['plan.json','state.json']) assert(!fs.readFileSync(path.join(stateDir,name),'utf8').includes('BROWSER_PASSWORD_CANARY'));
  assert(!fs.readFileSync(logFile,'utf8').includes('BROWSER_PASSWORD_CANARY'));
  // Emulate a process loss at a durable progress checkpoint, then reconnect to
  // the same address using the real saved browser cookie and startup recovery.
  await stop();
  const stateFile=path.join(stateDir,'state.json');const state=JSON.parse(fs.readFileSync(stateFile));
  Object.assign(state,{running:true,status:'running',phase:'Deploy Components',progress:63,error:'',applied_nodes:0});
  fs.writeFileSync(stateFile,JSON.stringify(state));await start();await page.reload();
  await page.waitForFunction(()=>document.querySelector('#progressValue')?.textContent==='63%');
  assert.equal((await (await context.request.get(origin+'/api/v1/status')).json()).status,'interrupted');
  await page.locator('.verbose summary').click();assert.equal(await page.locator('.verbose').getAttribute('open'),null);
  await Promise.all([page.waitForResponse(r=>r.url().endsWith('/api/v1/status')),page.click('#refreshNow')]);
  assert.equal(await page.locator('.verbose').getAttribute('open'),null,'status update reopened verbose output');
  // A temporary polling failure must leave the current durable progress visible.
  await page.route('**/api/v1/status',route=>route.abort());await page.click('#refreshNow');
  await page.waitForFunction(()=>document.querySelector('#toast').textContent.includes('Connection interrupted'));
  assert.equal(await page.textContent('#progressValue'),'63%');await page.unroute('**/api/v1/status');
  await page.click('#refreshNow');await page.locator('.verbose summary').click();
  await page.screenshot({path:'/tmp/titanus-web-installer-desktop.png',fullPage:true});
  assert.deepEqual(errors,[]);
  const result=await context.request.post(origin+'/api/v1/quiesce',{data:{}});assert.equal(result.status(),200);
  assert.equal((await context.request.post(origin+'/api/v1/start',{data:{apply:false}})).status(),409);
  console.log('TITANUS_WEB_INSTALLER_HTTPS_RECONNECT_OK');
 }finally{await stop();if(browser)await browser.close();fs.rmSync(directory,{recursive:true,force:true});}
})().catch(error=>{console.error(error);process.exitCode=1;});
