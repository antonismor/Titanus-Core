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
    return `<div class="field"><label for="${id}">${label}</label><input id="${id}" class="input" type="${type}" value="${esc(value || "")}" autocomplete="off"><small>${help}</small></div>