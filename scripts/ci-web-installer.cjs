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
  const credit='Design And Development by : antonios.mortos@outlook.com';
  assert.equal((await page.textContent('.developer-credit')).trim(),credit);
  const token=fs.readFileSync(path.join(stateDir,'access.token'),'utf8').trim();
  await page.goto(`${origin}/#access=${token}`);await page.locator('#nodeCount').waitFor();
  assert.equal(new URL(page.url()).hash,'','access fragment was retained');
  assert((await context.cookies()).some(c=>c.name==='titanus_installer_session'&&c.httpOnly&&c.secure&&c.sameSite==='Strict'));
  assert((await page.locator('body').innerText()).includes('SOON AVAILABLE'));
  // Native HA must preserve explicitly selected STORAGE-only nodes across rerenders.
  await page.click('[data-profile="native-ha"]');await page.selectOption('#nodeCount','9');await page.click('#toInfrastructure');
  const storageCap=page.locator('input[data-cap-index="6"][data-cap="STORAGE"]');
  const executionCap=page.locator('input[data-cap-index="6"][data-cap="EXECUTION"]');
  await storageCap.check();await executionCap.uncheck();await page.click('#backConfig');await page.click('#toInfrastructure');
  assert.equal(await storageCap.isChecked(),true);assert.equal(await executionCap.isChecked(),false);
  for(let n=0;n<9;n++){
   await page.fill(`#nodeName${n}`,n<3?`ctl0${n+1}`:n<6?`wrk0${n-2}`:`ceph0${n-5}`);
   await page.fill(`#nodeMgmt${n}`,`192.0.2.${10+n}`);
   if(n>=6){await page.locator(`input[data-cap-index="${n}"][data-cap="STORAGE"]`).check();await page.locator(`input[data-cap-index="${n}"][data-cap="EXECUTION"]`).uncheck();}
  }
  await page.fill('#sshPassword','BROWSER_PASSWORD_CANARY');await page.click('#toComponents');
  await page.locator('#cephEnabled').check();
  await page.locator('#toast').waitFor({state:'hidden'});
  await page.screenshot({path:'/tmp/titanus-web-installer-step3.png',fullPage:false});
  // Simulated probe response checks browser hydration only; no target hosts are contacted.
  await page.route('**/api/v1/autofill',async route=>{
   const request=route.request().postDataJSON();assert.equal(request.password,'BROWSER_PASSWORD_CANARY');
   assert.equal(request.plan._nodeCount,undefined);
   request.plan.ceph.public_cidr='192.0.2.0/24';request.plan.ceph.cluster_cidr='192.0.2.0/24';
   await route.fulfill({json:{plan:request.plan,findings:['Browser fixture: common storage network detected'],warnings:[]}});
  });
  await page.click('#autoQueryButton');await page.waitForFunction(()=>document.querySelector('#cephPublicCIDR').value==='192.0.2.0/24');
  assert.equal(await page.locator('#autoQueryButton').isEnabled(),true);
  await page.unroute('**/api/v1/autofill');
  await page.click('#backInfrastructure');
  for(let n=6;n<9;n++) assert.equal(await page.locator(`input[data-cap-index="${n}"][data-cap="EXECUTION"]`).isChecked(),false);
  await page.click('#backConfig');await page.click('[data-profile="standard"]');
  await page.selectOption('#nodeCount','1');await page.fill('#realmName','BROWSER-REALM');
  await page.click('#toInfrastructure');await page.fill('#nodeMgmt0','192.0.2.10');
  await page.fill('#sshPassword','BROWSER_PASSWORD_CANARY');await page.click('#toComponents');
  assert.equal(await page.locator('#autoQueryButton').isVisible(),true,'Step 3 auto-query button missing');
  assert.equal(await page.locator('#autoRepair').isChecked(),false,'automatic repair must be opt-in');
  await page.locator('#autoRepair').check();
  await page.click('#backInfrastructure');await page.click('#toComponents');
  assert.equal(await page.locator('#autoRepair').isChecked(),true,'repair choice lost on navigation');
  await page.locator('#autoRepair').scrollIntoViewIfNeeded();
  await page.screenshot({path:'/tmp/titanus-web-installer-auto-repair.png',fullPage:false});
  await page.fill('#releaseVersion','0.4.0-rc.17');await page.fill('#revision','a'.repeat(40));
  await page.fill('#amd64Path',path.join(directory,'absent-amd64.tar.gz'));await page.fill('#amd64SHA','b'.repeat(64));
  await page.fill('#arm64Path',path.join(directory,'absent-arm64.tar.gz'));await page.fill('#arm64SHA','c'.repeat(64));
  await page.click('#toReview');
  assert((await page.textContent('#autoRepairStatus')).includes('Enabled'));
  await page.click('#prepareOnly');
  await page.waitForFunction(()=>document.querySelector('#progressMessage')?.textContent.includes('stopped safely'));
  const planResponse=await context.request.get(origin+'/api/v1/plan');assert.equal(planResponse.status(),200);
  const plan=await planResponse.json();assert.equal(plan.realm_name,'BROWSER-REALM');assert.equal(plan.nodes.length,1);
  assert.equal(plan.installation.auto_repair,true,'repair choice not persisted in validated plan');
  for(const name of ['plan.json','state.json']) assert(!fs.readFileSync(path.join(stateDir,name),'utf8').includes('BROWSER_PASSWORD_CANARY'));
  assert(!fs.readFileSync(logFile,'utf8').includes('BROWSER_PASSWORD_CANARY'));
  // Emulate a process loss at a durable progress checkpoint, then reconnect to
  // the same address using the real saved browser cookie and startup recovery.
  await stop();
  const stateFile=path.join(stateDir,'state.json');const state=JSON.parse(fs.readFileSync(stateFile));
  Object.assign(state,{running:true,status:'running',phase:'Deploy Components',progress:63,error:'',applied_nodes:0,completed:false});
  delete state.report;
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
  // Browser-only active-run fixture: a manual click must queue a fresh request
  // after an already pending automatic poll, refresh logs and show feedback.
  const bootstrap=await (await context.request.get(origin+'/api/v1/bootstrap')).json();
  let fixture={...state,running:true,status:'running',phase:'Deploy Components',progress:72,prepared_only:false,total_nodes:1,applied_nodes:0,deployment_path:'/private/documentation-fixture'};
  let statusRequests=0,logRequests=0,releasePoll;
  const heldPoll=new Promise(resolve=>releasePoll=resolve);
  await page.route('**/api/v1/bootstrap',route=>route.fulfill({json:{...bootstrap,state:fixture}}));
  await page.route('**/api/v1/status',async route=>{
   const number=++statusRequests;const snapshot={...fixture,progress:number===1?64:fixture.progress};
   if(number===1)await heldPoll;
   await route.fulfill({json:snapshot});
  });
  await page.route('**/api/v1/logs',route=>{logRequests++;return route.fulfill({json:{lines:[`[ACCEPTANCE] Read-only browser fixture output ${logRequests}`]}});});
  await page.reload();await page.waitForFunction(()=>document.querySelector('#refreshNow'));
  await page.click('#refreshNow');assert.equal(await page.textContent('#refreshNow'),'Refreshing…');
  releasePoll();await page.waitForFunction(()=>document.querySelector('#lastRefresh')?.textContent.startsWith('Last refresh:'));
  assert.equal(await page.textContent('#progressValue'),'72%');assert(statusRequests>=2);assert(logRequests>=2);
  const checks=[{node:'node01',name:'Titanus services',status:'Passed',attempts:1,details:'titanusd and titanus-agent are active'},
   {node:'node01',name:'HTTPS identity, certificates and release',status:'Passed',attempts:1,details:'Verified HTTPS/mTLS and exact selected release'},
   {node:'node01',name:'Realm membership, roles and Fabric',status:'Running',attempts:1,details:'Read-only probe 1/3'},
   {node:'Realm',name:'Controller quorum',status:'Pending',attempts:0,details:''}];
  fixture={...fixture,status:'acceptance',phase:'Complete',progress:96,applied_nodes:1,message:'Running read-only cluster acceptance checks.',report:{format:'titanus-installation-report/v1',id:'browser-report',session_id:state.session_id,scope:'cluster',realm:plan.realm_name,revision:plan.installation.revision,status:'Running',checks_read_only:true,checks}};
  await page.click('#refreshNow');await page.locator('#reportStatus').waitFor();
  assert.equal(await page.textContent('#reportStatus'),'Running');
  assert((await page.textContent('.step.active')).includes('Complete'));
  assert.equal(await page.locator('.verbose').getAttribute('open'),'');
  assert((await page.textContent('#terminal')).includes('[ACCEPTANCE]'));
  await page.locator('.verbose summary').click();
  checks.forEach(c=>{c.status='Passed';c.attempts=1;c.details=c.details||'Standalone CONTROL node responds';});
  checks[2].details='1/1 expected nodes READY with fresh pulses, matching roles and Fabric';
  fixture={...fixture,status:'completed',progress:100,message:'Installation and cluster acceptance completed successfully.',report:{...fixture.report,status:'Success',checks}};
  await page.click('#refreshNow');await page.waitForFunction(()=>document.querySelector('#reportStatus').textContent==='Success');
  assert.equal(await page.locator('#newInstall').isEnabled(),false,'cleanup reservation lost during completion');
  assert.equal(await page.locator('.verbose').getAttribute('open'),null,'report update reopened verbose output');
  fixture.running=false;await page.click('#refreshNow');await page.locator('.verbose summary').click();
  assert.equal(await page.locator('#newInstall').isEnabled(),true);
  assert.equal((await page.textContent('.developer-credit')).trim(),credit);
  await page.screenshot({path:'/tmp/titanus-web-installer-complete.png',fullPage:true});
  fixture={...fixture,status:'failed',completed:false,error:'Cluster acceptance failed.',message:'Installation finished with failed acceptance checks.',report:{...fixture.report,status:'Failed',checks:[{...checks[0],status:'Failed',details:'titanus-agent is not active',attempts:3},...checks.slice(1)]}};
  await page.click('#refreshNow');await page.waitForFunction(()=>document.querySelector('#reportStatus').textContent==='Failed');
  await page.reload();await page.waitForFunction(()=>document.querySelector('#reportStatus')?.textContent==='Failed');
  let acceptanceStarts=0,unexpectedInstalls=0;
  await page.route('**/api/v1/start',route=>{unexpectedInstalls++;return route.fulfill({status:409,json:{error:'Never replay installation'}});});
  await page.route('**/api/v1/acceptance',route=>{assert.equal(route.request().method(),'POST');acceptanceStarts++;fixture={...fixture,running:true,status:'acceptance',report:{...fixture.report,status:'Running'}};return route.fulfill({json:fixture});});
  await page.click('#runAcceptance');await page.waitForFunction(()=>document.querySelector('#reportStatus').textContent==='Running');
  assert.equal(acceptanceStarts,1);assert.equal(unexpectedInstalls,0);
  assert.deepEqual(errors,[]);
  const result=await context.request.post(origin+'/api/v1/quiesce',{data:{}});assert.equal(result.status(),200);
  assert.equal((await context.request.post(origin+'/api/v1/start',{data:{apply:false}})).status(),409);
  console.log('TITANUS_WEB_INSTALLER_HTTPS_RECONNECT_OK');
 }finally{await stop();if(browser)await browser.close();fs.rmSync(directory,{recursive:true,force:true});}
})().catch(error=>{console.error(error);process.exitCode=1;});
