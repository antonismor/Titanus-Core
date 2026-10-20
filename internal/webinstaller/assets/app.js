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

  function clo