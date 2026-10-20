(() => {
  "use strict";

  const steps = [
    ["Configuration", "Set up your deployment"],
    ["Infrastructure", "Add and verify nodes"],
    ["Components", "Select and configure"],
    ["Review", "Check settings"],
    ["Deployment", "Install and monitor"],
    ["Complete", "Access your environment"]
  ];

  const model = {
    token: "",
    meta: null,
    currentStep: 1,
    validated: false,
    fingerprints: [],
    password: "",
    startedAt: 0,
    statusTimer: null,
    logTimer: null,
    state: null,
    plan: null
  };

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const esc = (value) => String(value ?? "").replace(/[&<>'"]/g, ch => ({"&":"&amp;","<":"&lt;",">":"&gt;","'":"&#39;",'"':"&quot;"}[ch]));

  function readAccessToken() {
    const hash = new URLSearchParams(location.hash.replace(/^#/, ""));
    model.token = hash.get("access") || "";
    if (model.token) history.replaceState(null, "", location.pathname + location.search);
  }

  async function api(path, options = {}) {
    const headers = Object.assign({}, options.headers || {});
    if (model.token) headers["X-Titanus-Installer-Token"] = model.token;
    if (options.body && !headers["Content-Type"]) headers["Content-Type"] = "application/json";
    const response = await fetch(path, Object.assign({}, options, {headers, cache: "no-store"}));
    const payload = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(payload.error || `Request failed (${response.status})`);
    return payload;
  }

  function makeDefaultPlan(meta) {
    const revision = /^[0-9a-f]{40}$/.test(meta.revision || "") ? meta.revision : "";
    return {
      version: "titanus-plan/v2",
      realm_name: "TITANUS-REALM",
      fabric_cidr: "10.210.0.0/16",
      service_cidr: "10.220.0.0/16",
      created_at: new Date().toISOString(),
      nodes: [],
      ceph: {enabled:false, replication:1, enable_rbd:true, enable_cephfs:true, enable_rgw:false, provision:false},
      auto_deploy: false,
      installation: {
        profile: "standard",
        operation: "install",
        release_version: meta.version || "0.4.0-rc.17",
        revision,
        archives: {
          amd64: {path:"", sha256:""},
          arm64: {path:"", sha256:""}
        },
        pki_dir: "/var/lib/titanus/installer-web/pki/TITANUS-REALM",
        api_port: 9443,
        raft_port: 9444,
        node_prefix: 24,
        vxlan_id: 4242,
        start: true
      }
    };
  }

  function profileLabel() {
    return ({"standard":"Standard","native-ha":"Native HA","custom":"Advanced / Custom"})[model.plan.installation.profile] || model.plan.installation.profile;
  }

  function ensureNodes() {
    const count = Math.max(1, Math.min(model.meta.community_node_limit, Number(model.plan._nodeCount || 3)));
    model.plan._nodeCount = count;
    while (model.plan.nodes.length < count) {
      const i = model.plan.nodes.length;
      model.plan.nodes.push({
        name: `titanus${String(i+1).padStart(2,"0")}`,
        management_ip: "",
        ssh_user: model.plan._sshUser || "root",
        ssh_port: Number(model.plan._sshPort || 22),
        capabilities: ["EXECUTION"]
      });
    }
    model.plan.nodes = model.plan.nodes.slice(0, count);
    const controls = validControlCount(Number(model.plan._controlCount || (count >= 3 ? 3 : 1)), count);
    model.plan._controlCount = controls;
    model.plan.nodes.forEach((node, i) => {
      node.ssh_user = model.plan._sshUser || node.ssh_user || "root";
      node.ssh_port = Number(model.plan._sshPort || node.ssh_port || 22);
      if (!node.capabilities || !node.capabilities.length) node.capabilities = ["EXECUTION"];
      if (model.plan.installation.profile === "native-ha") {
        node.capabilities = i < controls ? ["CONTROL"] : Array.from(new Set(node.capabilities.filter(c => c !== "CONTROL").concat("EXECUTION")));
        if (i < controls) node.capabilities = node.capabilities.filter(c => c !== "EXECUTION");
      } else {
        if (i < controls && !node.capabilities.includes("CONTROL")) node.capabilities.push("CONTROL");
        if (i >= controls) node.capabilities = node.capabilities.filter(c => c !== "CONTROL");
      }
    });
  }

  function validControlCount(wanted, nodeCount) {
    const options = [1,3,5].filter(n => n <= nodeCount);
    if (model.plan?.installation?.profile === "native-ha") {
      const native = [3,5].filter(n => n <= nodeCount - 2);
      return native.includes(wanted) ? wanted : (native[0] || 3);
    }
    return options.includes(wanted) ? wanted : (nodeCount >= 3 ? 3 : 1);
  }

  function normalizePlanForAPI(p) {
    const install = p.installation;
    p.created_at = new Date().toISOString();
    p.realm_name = (p.realm_name || "TITANUS-REALM").trim();
    install.pki_dir = install.pki_dir || `/var/lib/titanus/installer-web/pki/${p.realm_name}`;
    if (install.profile === "native-ha") {
      if (install._fencing_error) throw new Error("Native HA fencing JSON is invalid: " + install._fencing_error);
      install.operation = "install";
      p.ceph.enable_rgw = false;
      if (p.ceph.enabled) p.ceph.replication = 3;
    }
    if (!p.ceph.enabled) {
      p.ceph = {enabled:false, replication:1, enable_rbd:true, enable_cephfs:true, enable_rgw:false, provision:false};
      p.nodes.forEach(n => { delete n.ceph_public_ip; delete n.ceph_cluster_ip; delete n.ceph_devices; n.capabilities = n.capabilities.filter(c => c !== "STORAGE"); });
    }
    if (install.operation === "rollback") delete install.archives;
    else if (!install.archives) install.archives = {amd64:{path:"",sha256:""},arm64:{path:"",sha256:""}};
    if (install.profile !== "native-ha") delete install.power_fencing;
    delete install._fencing_raw;
    delete install._fencing_error;
    delete p._nodeCount;
    delete p._controlCount;
    delete p._sshUser;
    delete p._sshPort;
    return p;
  }

  function clonePlanForAPI() {
    ensureNodes();
    const copy = JSON.parse(JSON.stringify(model.plan));
    return normalizePlanForAPI(copy);
  }

  function stepNav() {
    const nav = $("#stepNav");
    nav.innerHTML = steps.map((step, i) => {
      const n = i + 1;
      const cls = n === model.currentStep ? "step active" : n < model.currentStep ? "step done" : "step";
      return `<div class="${cls}"><div class="bubble"><span>${n}</span></div><div><div class="step-title">${step[0]}</div><div class="step-sub">${step[1]}</div></div></div>`;
    }).join("");
  }

  function updateSummary() {
    if (!model.plan) return;
    const p = model.plan;
    const install = p.installation;
    $("#summaryList").innerHTML = `
      <dt>Edition</dt><dd>Community</dd>
      <dt>Type</dt><dd>${esc(profileLabel())}</dd>
      <dt>Operation</dt><dd>${esc(install.operation || "install")}</dd>
      <dt>Nodes</dt><dd>${p._nodeCount || p.nodes.length || 0}</dd>
      <dt>Realm</dt><dd>${esc(p.realm_name || "-")}</dd>
      <dt>Fabric</dt><dd>${esc(p.fabric_cidr || "-")}</dd>
      <dt>Ceph</dt><dd>${p.ceph.enabled ? "Enabled" : "Disabled"}</dd>`;
    const caps = ["Titanus Core","SSH full-mesh","Native Fabric","Versioned releases"];
    if (p.ceph.enabled) caps.push("Distributed Storage (Ceph)");
    if (install.profile === "native-ha") caps.push("Native HA / Fencing");
    caps.push("All TUI installation options");
    $("#summaryDetails").innerHTML = `<h3>Available Components</h3><div class="checklist">${caps.map(x => `<div class="checkline"><span class="checkdot">✓</span>${esc(x)}</div>`).join("")}</div>`;
  }

  function render() {
    stepNav();
    updateSummary();
    if (model.currentStep === 1) renderConfiguration();
    else if (model.currentStep === 2) renderInfrastructure();
    else if (model.currentStep === 3) renderComponents();
    else if (model.currentStep === 4) renderReview();
    else if (model.currentStep === 5) renderDeployment();
    else renderComplete();
  }

  function header(title, subtitle, step) {
    return `<div class="eyebrow">STEP ${step} OF 6</div><h1>${title}</h1><p class="lead">${subtitle}</p>`;
  }

  function renderConfiguration() {
    const p = model.plan;
    const i = p.installation;
    $("#workspace").innerHTML = header("Deployment Configuration", "Choose your installation type and basic settings. Everything available in the TUI installer is available in this Web Installer.", 1) + `
      <div class="card">
        <h3>Edition</h3>
        <div class="grid-2">
          <div class="choice active"><span class="choice-tag">ACTIVE</span><div class="choice-icon">◈</div><h3>Community Edition</h3><p>Up to ${model.meta.community_node_limit} Realm nodes. Units, Fleets and applications do not consume node slots.</p></div>
          <div class="choice disabled"><span class="choice-tag">SOON AVAILABLE</span><div class="choice-icon">◇</div><h3>Enterprise Edition</h3><p>Same installer experience, planned without an edition-imposed node ceiling and with a 90-day trial.</p></div>
        </div>
        <div class="sep"></div>
        <h3>Installation Type</h3>
        <div class="grid-3" id="profileChoices">
          ${profileCard("standard","▤","Standard","General Titanus Core deployment with a guided topology.")}
          ${profileCard("native-ha","⬡","Native HA","Fresh 5–9 node deployment with separate voters, Workers and verified fencing.")}
          ${profileCard("custom","⚙","Advanced / Custom","Manually control node capabilities, storage and low-level settings.")}
        </div>
        <div class="grid-2" style="margin-top:20px">
          <div class="field"><label>Operation</label><select id="operation" class="select">
            <option value="install" ${i.operation==="install"?"selected":""}>Initial install</option>
            <option value="upgrade" ${i.operation==="upgrade"?"selected":""} ${i.profile==="native-ha"?"disabled":""}>Upgrade</option>
            <option value="rollback" ${i.operation==="rollback"?"selected":""} ${i.profile==="native-ha"?"disabled":""}>Binary rollback</option>
          </select><small>Native HA remains a fresh-install profile; existing-Realm transition uses the separate verified HA workflow.</small></div>
          <div class="field"><label>Deployment Size</label><select id="nodeCount" class="select">${Array.from({length:model.meta.community_node_limit},(_,idx)=>idx+1).map(n=>`<option value="${n}" ${(p._nodeCount||3)==n?"selected":""} ${i.profile==="native-ha"&&n<5?"disabled":""}>${n} Node${n>1?"s":""}</option>`).join("")}</select><small>Community Edition supports up to ${model.meta.community_node_limit} nodes.</small></div>
        </div>
        <div class="grid-2">
          ${field("Realm Name","realmName",p.realm_name,"Unique identity for this Titanus Realm.")}
          ${field("Persistent PKI Directory","pkiDir",i.pki_dir,"Private server-side path for the Realm authority.")}
        </div>
        <div class="grid-2">
          ${field("Titanus Fabric CIDR","fabricCIDR",p.fabric_cidr,"Workload Fabric address space.")}
          ${field("Titanus Service CIDR","serviceCIDR",p.service_cidr,"Service address space; must not overlap the Fabric.")}
        </div>
        <div class="toolbar"><span class="muted small">Enterprise is displayed here but remains unavailable until its entitlement model is implemented.</span><button id="toInfrastructure" class="btn primary">Next&nbsp; →</button></div>
      </div>`;
    bindConfiguration();
  }

  function profileCard(value, icon, title, text) {
    return `<button class="choice ${model.plan.installation.profile===value?"active":""}" data-profile="${value}" type="button"><div class="choice-icon">${icon}</div><h3>${title}</h3><p>${text}</p></button>`;
  }

  function field(label, id, value, help, type="text") {
    return `<div class="field"><label for="${id}">${label}</label><input id="${id}" class="input" type="${type}" value="${esc(value || "")}" autocomplete="off"><small>${help}</small></div>`;
  }

  function bindConfiguration() {
    $$('[data-profile]').forEach(el => el.addEventListener("click", () => {
      model.plan.installation.profile = el.dataset.profile;
      if (el.dataset.profile === "native-ha") {
        model.plan.installation.operation = "install";
        model.plan._nodeCount = Math.max(5, Number(model.plan._nodeCount || 5));
        model.plan._controlCount = 3;
      }
      ensureNodes(); render();
    }));
    $("#operation").addEventListener("change", e => { model.plan.installation.operation = e.target.value; render(); });
    $("#nodeCount").addEventListener("change", e => { model.plan._nodeCount = Number(e.target.value); ensureNodes(); updateSummary(); });
    bindText("realmName", v => { model.plan.realm_name=v; if (!model.plan.installation.pki_dir || model.plan.installation.pki_dir.includes("/pki/")) model.plan.installation.pki_dir=`/var/lib/titanus/installer-web/pki/${v}`; });
    bindText("pkiDir", v => model.plan.installation.pki_dir=v);
    bindText("fabricCIDR", v => model.plan.fabric_cidr=v);
    bindText("serviceCIDR", v => model.plan.service_cidr=v);
    $("#toInfrastructure").addEventListener("click", () => { ensureNodes(); model.currentStep=2; render(); });
  }

  function bindText(id, fn) { $("#"+id)?.addEventListener("input", e => { fn(e.target.value); model.validated=false; updateSummary(); }); }

  function renderInfrastructure() {
    ensureNodes();
    const p = model.plan;
    const controls = validControlCount(Number(p._controlCount || 3), p.nodes.length);
    const controlOptions = [1,3,5].filter(n => n <= p.nodes.length && (p.installation.profile !== "native-ha" || (n>=3 && n<=p.nodes.length-2)));
    $("#workspace").innerHTML = header("Infrastructure", "Add every VM or physical host, set SSH credentials, addresses and Titanus-native capabilities. Passwords stay only in browser memory until deployment starts.", 2) + `
      <div class="card">
        <div class="grid-3">
          <div class="field"><label>Common SSH Username</label><input id="sshUser" class="input" value="${esc(p._sshUser||"root")}" autocomplete="username"><small>Must resolve to UID 0 on every selected node.</small></div>
          <div class="field"><label>Common SSH Port</label><input id="sshPort" class="input" type="number" min="1" max="65535" value="${Number(p._sshPort||22)}"><small>Used for discovery, trust bootstrap and deployment.</small></div>
          <div class="field"><label>Initial SSH Password</label><input id="sshPassword" class="input" type="password" value="${esc(model.password)}" autocomplete="new-password"><small>Never written to the Plan, log, URL, environment or disk.</small></div>
        </div>
        <div class="grid-2">
          <div class="field"><label>CONTROL-capable Nodes</label><select id="controlCount" class="select">${controlOptions.map(n=>`<option value="${n}" ${controls===n?"selected":""}>${n}</option>`).join("")}</select><small>Voting topology must use 1, 3 or 5 CONTROL nodes.</small></div>
          <div class="field"><label>Node Count</label><input class="input" value="${p.nodes.length} / ${model.meta.community_node_limit}" disabled><small>Each VM or physical host counts once regardless of capabilities.</small></div>
        </div>
        ${p.installation.profile === "native-ha" ? `<div class="notice">Native HA separates CONTROL voters from EXECUTION Workers. The Web Installer enforces the same topology and fencing validation as the TUI.</div>` : ""}
      </div>
      <div id="nodesArea">${p.nodes.map((n,idx)=>nodeCard(n,idx)).join("")}</div>
      <div class="toolbar"><button class="btn" id="backConfig">← Back</button><button class="btn primary" id="toComponents">Next&nbsp; →</button></div>`;
    bindInfrastructure();
  }

  function nodeCard(node, idx) {
    const allCaps=["CONTROL","EXECUTION","STORAGE","GATEWAY","GPU","BACKUP"];
    const native=model.plan.installation.profile === "native-ha";
    return `<div class="node-card"><div class="node-head"><strong>Node ${idx+1} / ${model.plan.nodes.length}</strong><span class="muted small">${esc(node.name||"")}</span></div>
      <div class="grid-3">
        ${field("Hostname",`nodeName${idx}`,node.name,"Titanus node identity.")}
        ${field("Management IP",`nodeMgmt${idx}`,node.management_ip,"IPv4 address used by the installer.")}
        ${field("Dedicated Fabric IP (optional)",`nodeFabric${idx}`,node.fabric_ip||"","Leave blank to use management routing.")}
      </div>
      <div class="field"><label>Capabilities</label><div class="capabilities">${allCaps.map(cap=>`<label class="cap"><input type="checkbox" data-cap-index="${idx}" data-cap="${cap}" ${node.capabilities.includes(cap)?"checked":""} ${(native&&((idx<model.plan._controlCount&&(cap==="EXECUTION"))||(idx>=model.plan._controlCount&&(cap==="CONTROL"))))?"disabled":""}>${cap}</label>`).join("")}</div></div>
      ${model.plan.ceph.enabled && node.capabilities.includes("STORAGE") ? `<div class="grid-3">${field("Ceph Public IP",`cephPublic${idx}`,node.ceph_public_ip||"","Public storage network address.")}${field("Ceph Cluster IP",`cephCluster${idx}`,node.ceph_cluster_ip||"","Replication network address.")}${field("Ceph Devices",`cephDevices${idx}`,(node.ceph_devices||[]).join(","),"Comma-separated /dev/... paths.")}</div>` : ""}
    </div>`;
  }

  function bindInfrastructure() {
    bindText("sshUser", v => { model.plan._sshUser=v; model.plan.nodes.forEach(n=>n.ssh_user=v); });
    $("#sshPort").addEventListener("input", e => { const v=Number(e.target.value||22); model.plan._sshPort=v; model.plan.nodes.forEach(n=>n.ssh_port=v); });
    $("#sshPassword").addEventListener("input", e => model.password=e.target.value);
    $("#controlCount").addEventListener("change", e => { model.plan._controlCount=Number(e.target.value); ensureNodes(); render(); });
    model.plan.nodes.forEach((node,idx)=>{
      bindText(`nodeName${idx}`,v=>node.name=v);
      bindText(`nodeMgmt${idx}`,v=>node.management_ip=v);
      bindText(`nodeFabric${idx}`,v=> { if(v) node.fabric_ip=v; else delete node.fabric_ip; });
      bindText(`cephPublic${idx}`,v=> { if(v) node.ceph_public_ip=v; else delete node.ceph_public_ip; });
      bindText(`cephCluster${idx}`,v=> { if(v) node.ceph_cluster_ip=v; else delete node.ceph_cluster_ip; });
      bindText(`cephDevices${idx}`,v=> node.ceph_devices=v.split(",").map(x=>x.trim()).filter(Boolean));
    });
    $$('[data-cap-index]').forEach(el=>el.addEventListener("change",()=>{
      const node=model.plan.nodes[Number(el.dataset.capIndex)], cap=el.dataset.cap;
      if(el.checked && !node.capabilities.includes(cap)) node.capabilities.push(cap);
      if(!el.checked) node.capabilities=node.capabilities.filter(x=>x!==cap);
      model.validated=false;
      if(cap==="STORAGE") render(); else updateSummary();
    }));
    $("#backConfig").addEventListener("click",()=>{model.currentStep=1;render();});
    $("#toComponents").addEventListener("click",()=>{ model.currentStep=3; render(); });
  }

  function renderComponents() {
    const p=model.plan, i=p.installation, c=p.ceph;
    const isNative=i.profile==="native-ha";
    $("#workspace").innerHTML = header("Components & Advanced Settings", "Configure Ceph, release identity, ports, Fabric parameters and Native HA fencing. These are the same installation inputs exposed by the TUI.", 3) + `
      <div class="card">
        <h3>Distributed Storage</h3>
        <label class="switch-row"><input id="cephEnabled" type="checkbox" ${c.enabled?"checked":""}> <strong>Configure distributed Ceph storage</strong></label>
        ${c.enabled ? `<div class="grid-3">
          ${field("Ceph Public CIDR","cephPublicCIDR",c.public_cidr||"10.230.0.0/24","Client/public network.")}
          ${field("Ceph Cluster CIDR","cephClusterCIDR",c.cluster_cidr||"10.231.0.0/24","Replication network.")}
          ${field("Requested Usable TB","requestedTB",c.requested_usable_tb||10,"Capacity planning target.","number")}
        </div><div class="grid-3">
          ${field("Replication Copies","replication",isNative?3:(c.replication||3),isNative?"Native HA requires three copies.":"Must not exceed STORAGE nodes.","number")}
          <div class="field"><label>Storage Services</label><div class="capabilities"><label class="cap"><input id="rbd" type="checkbox" ${c.enable_rbd?"checked":""}>RBD</label><label class="cap"><input id="cephfs" type="checkbox" ${c.enable_cephfs?"checked":""}>CephFS</label><label class="cap"><input id="rgw" type="checkbox" ${c.enable_rgw?"checked":""} ${isNative?"disabled":""}>RGW</label></div><small>Native HA RGW HA remains unavailable.</small></div>
          <div class="field"><label>Provisioning</label><label class="switch-row"><input id="provisionCeph" type="checkbox" ${c.provision?"checked":""} ${i.operation!=="install"?"disabled":""}> Provision a new Ceph cluster</label><small>Only explicitly selected unused devices may be initialized.</small></div>
        </div>` : `<p class="muted small">Ceph can be enabled for Custom and Native HA profiles. Existing TUI safety rules remain enforced.</p>`}
      </div>
      ${isNative ? `<div class="card"><h3>Native HA Power Fencing</h3><div class="field"><label>Private Fencing Inventory JSON</label><textarea id="fencingJSON" spellcheck="false" placeholder='{"worker01":{"provider":"proxmox","uuid":"...","address":"https://...","user":"...","credential_file":"/root/...","ca_file":"/root/..."}}'>${esc(i._fencing_raw !== undefined ? i._fencing_raw : (i.power_fencing ? JSON.stringify(i.power_fencing,null,2) : ""))}</textarea><small>Same Target schema as the TUI inventory. Credential file contents are never placed in the Plan or browser.</small></div></div>` : ""}
      <div class="card">
        <h3>Verified Release</h3>
        <div class="grid-2">${field("Target Release Version","releaseVersion",i.release_version,"Exact release version.")}${field("Exact Tested Main SHA","revision",i.revision,"40-character tested Git revision.")}</div>
        ${i.operation!=="rollback" ? `<div class="grid-2">${field("AMD64 Native Archive","amd64Path",i.archives?.amd64?.path||"","Server-side path to the downloaded archive.")}${field("AMD64 Published SHA256","amd64SHA",i.archives?.amd64?.sha256||"","64-character checksum.")}</div><div class="grid-2">${field("ARM64 Native Archive","arm64Path",i.archives?.arm64?.path||"","Server-side path to the downloaded archive.")}${field("ARM64 Published SHA256","arm64SHA",i.archives?.arm64?.sha256||"","64-character checksum.")}</div>` : `<div class="notice">Binary rollback selects the verified previously installed release and therefore does not require downloaded archive paths.</div>`}
      </div>
      <div class="card">
        <h3>Advanced Network & Lifecycle</h3>
        <div class="grid-3">${field("mTLS API Port","apiPort",i.api_port,"Default 9443.","number")}${field("mTLS Raft Port","raftPort",i.raft_port,"Default 9444; distinct from API.","number")}${field("Fabric VXLAN ID","vxlanID",i.vxlan_id,"1–16777215.","number")}</div>
        <div class="grid-2">${field("Per-node Unit Subnet Prefix","nodePrefix",i.node_prefix,"16–30.","number")}<div class="field"><label>Activation</label><label class="switch-row"><input id="startServices" type="checkbox" ${i.start?"checked":""}> Start services when installation is applied</label><small>Same final TUI choice.</small></div></div>
      </div>
      <div class="toolbar"><button class="btn" id="backInfrastructure">← Back</button><button class="btn primary" id="toReview">Review&nbsp; →</button></div>`;
    bindComponents();
  }

  function bindComponents() {
    const p=model.plan, i=p.installation, c=p.ceph;
    $("#cephEnabled").addEventListener("change",e=>{c.enabled=e.target.checked; if(c.enabled){c.public_cidr=c.public_cidr||"10.230.0.0/24";c.cluster_cidr=c.cluster_cidr||"10.231.0.0/24";c.replication=i.profile==="native-ha"?3:(c.replication||3);c.requested_usable_tb=c.requested_usable_tb||10;} render();});
    bindText("cephPublicCIDR",v=>c.pub