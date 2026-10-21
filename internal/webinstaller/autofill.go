package webinstaller

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/release"
	"github.com/antonismor/Titanus-Core/internal/releasecache"
	"github.com/antonismor/Titanus-Core/internal/sshtrust"
)

const discoveryProfilePath = "/etc/titanus/installer-discovery.json"

type autoQueryRequest struct {
	Plan     model.RealmPlan `json:"plan"`
	Password string          `json:"password"`
}

type autoQueryResponse struct {
	Plan     model.RealmPlan `json:"plan"`
	Findings []string        `json:"findings"`
	Warnings []string        `json:"warnings,omitempty"`
}

type discoveryProfile struct {
	PowerFencing map[string]fencing.Target `json:"power_fencing,omitempty"`
	ReleaseDirs  []string                  `json:"release_dirs,omitempty"`
}

type probeDevice struct {
	Path string `json:"path"`
	Size uint64 `json:"size"`
}

type nodeProbe struct {
	Node     string        `json:"node"`
	UUID     string        `json:"uuid,omitempty"`
	CIDRs    []string      `json:"cidrs,omitempty"`
	Devices  []probeDevice `json:"devices,omitempty"`
	Existing bool          `json:"existing_installation,omitempty"`
}

const readOnlyProbePython = `import glob,json,os,subprocess
def out(args):
    return subprocess.check_output(args,text=True,stderr=subprocess.DEVNULL,timeout=10)
if os.geteuid()!=0:
    raise SystemExit("Titanus auto-query requires UID 0 on the selected node")
uuid=""
try:
    uuid=open("/sys/class/dmi/id/product_uuid").read().strip().lower()
except Exception:
    pass
cidrs=[]
for iface in json.loads(out(["ip","-j","-4","address","show"])):
    for a in iface.get("addr_info",[]):
        if a.get("family")!="inet" or a.get("scope")!="global":
            continue
        local=a.get("local",""); prefix=a.get("prefixlen")
        if local and prefix is not None:
            cidrs.append("%s/%s"%(local,prefix))
tree=json.loads(out(["lsblk","-J","-b","-o","NAME,PATH,TYPE,SIZE,FSTYPE,MOUNTPOINTS"]))["blockdevices"]
def busy(d):
    if d.get("fstype"): return True
    if any(x for x in (d.get("mountpoints") or []) if x): return True
    return any(busy(c) for c in (d.get("children") or []))
aliases=glob.glob("/dev/disk/by-id/*")
def alias_for(path):
    found=[]
    rp=os.path.realpath(path)
    for a in aliases:
        base=os.path.basename(a)
        if "-part" in base: continue
        try:
            if os.path.realpath(a)==rp: found.append(a)
        except OSError:
            pass
    def rank(a):
        b=os.path.basename(a)
        return (0 if b.startswith("wwn-") else 1 if b.startswith("scsi-") else 2 if b.startswith("nvme-") else 3 if b.startswith("ata-") else 4, len(a), a)
    return sorted(found,key=rank)[0] if found else path
devices=[]
for d in tree:
    if d.get("type")!="disk" or busy(d) or d.get("children"):
        continue
    path=d.get("path")
    if not path: continue
    p=subprocess.run(["wipefs","--no-act","--noheadings","--output","TYPE",path],text=True,capture_output=True,timeout=10)
    if p.returncode!=0 or p.stdout.strip():
        continue
    devices.append({"path":alias_for(path),"size":int(d.get("size") or 0)})
existing=bool(glob.glob("/etc/ceph/*.conf")) or any(os.path.exists(p) for p in ("/etc/titanus/daemon.env","/etc/titanus/agent.env","/var/lib/titanus/realm/state.json","/var/lib/titanus/realm/consensus"))
for unit in ("titanusd", "titanus-agent"):
    if subprocess.run(["systemctl","is-active","--quiet",unit],capture_output=True,timeout=5).returncode==0:
        existing=True
print(json.dumps({"uuid":uuid,"cidrs":sorted(set(cidrs)),"devices":devices,"existing_installation":existing},sort_keys=True))
`

func (s *Server) handleAutoQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if r.Context().Err() != nil {
		return
	}
	if s.quiescing {
		writeError(w, http.StatusConflict, "installer is switching to terminal-only mode")
		return
	}
	s.mu.RLock()
	busy := s.running
	s.mu.RUnlock()
	if busy {
		writeError(w, http.StatusConflict, "an installation is already running")
		return
	}

	var req autoQueryRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	req.Plan.Normalize()
	if err := validateAutoQueryInventory(req.Plan); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Password == "" || len(req.Password) > 4096 || strings.ContainsAny(req.Password, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "enter the initial SSH password in Step 2 before querying infrastructure")
		return
	}
	if err := sshtrust.CheckTools(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	password := []byte(req.Password)
	defer func() {
		for i := range password {
			password[i] = 0
		}
		req.Password = ""
	}()

	bootstrap := s.bootstrapFor(req.Plan)
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	hosts, err := bootstrap.Discover(ctx, req.Plan.Nodes)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read-only SSH identity discovery failed: "+err.Error())
		return
	}
	probes, err := s.probeNodes(ctx, hosts, password)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	profile, profileWarnings := loadDiscoveryProfile()
	stored, storedErr := fencing.LoadTargets(filepath.Join(s.cfg.StateDir, "fencing-profile.json"), true)
	if storedErr == nil {
		if profile.PowerFencing == nil {
			profile.PowerFencing = map[string]fencing.Target{}
		}
		for name, target := range stored {
			profile.PowerFencing[name] = target
		}
	} else if !os.IsNotExist(storedErr) {
		profileWarnings = append(profileWarnings, "Saved fencing profile needs attention: "+storedErr.Error())
	}

	findings, warnings := applyAutoQuery(&req.Plan, probes, profile)
	if req.Plan.Installation != nil {
		for name, target := range req.Plan.Installation.PowerFencing {
			if target.Provider != "proxmox" || fencing.ValidateTargets(map[string]fencing.Target{name: target}, true) != nil {
				continue
			}
			if err := (&fencing.Power{Targets: map[string]fencing.Target{name: target}}).ProbeProxmoxContext(ctx, name); err != nil {
				warnings = append(warnings, name+" Proxmox API readiness: "+err.Error())
			} else {
				findings = append(findings, name+": Proxmox TLS, VM UUID and effective power permissions verified with read-only requests")
			}
		}
	}

	warnings = append(profileWarnings, warnings...)
	archiveFindings, archiveWarnings := autoFillArchives(&req.Plan, profile)
	findings = append(findings, archiveFindings...)
	warnings = append(warnings, archiveWarnings...)
	s.logf("INFO", "Read-only infrastructure auto-query completed for %d nodes; no remote changes were made", len(req.Plan.Nodes))
	writeJSON(w, http.StatusOK, autoQueryResponse{Plan: req.Plan, Findings: uniqueStrings(findings), Warnings: uniqueStrings(warnings)})
}

func validateAutoQueryInventory(plan model.RealmPlan) error {
	if len(plan.Nodes) < 1 || len(plan.Nodes) > model.CommunityNodeLimit {
		return fmt.Errorf("auto-query requires 1-%d nodes", model.CommunityNodeLimit)
	}
	seen := map[string]bool{}
	for _, node := range plan.Nodes {
		ip := net.ParseIP(node.ManagementIP)
		if node.Name == "" || ip == nil || ip.To4() == nil || node.SSHUser == "" || node.SSHPort < 1 || node.SSHPort > 65535 {
			return fmt.Errorf("complete node names, management IPv4 addresses and SSH settings before auto-query")
		}
		if seen[node.Name] || seen[ip.String()] {
			return fmt.Errorf("auto-query refuses duplicate node names or management addresses")
		}
		seen[node.Name], seen[ip.String()] = true, true
	}
	return nil
}

func (s *Server) probeNodes(ctx context.Context, hosts []sshtrust.Host, password []byte) (map[string]nodeProbe, error) {
	dir, err := os.MkdirTemp(s.cfg.StateDir, ".auto-query-")
	if err != nil {
		return nil, fmt.Errorf("cannot create private auto-query workspace")
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, fmt.Errorf("cannot secure auto-query workspace")
	}
	known := filepath.Join(dir, "known_hosts")
	var lines strings.Builder
	for _, host := range hosts {
		label := host.Node.ManagementIP
		if host.Node.SSHPort != 22 {
			label = "[" + host.Node.ManagementIP + "]:" + strconv.Itoa(host.Node.SSHPort)
		}
		fmt.Fprintf(&lines, "%s %s\n", label, host.Key)
	}
	if err := os.WriteFile(known, []byte(lines.String()), 0600); err != nil {
		return nil, fmt.Errorf("cannot create private host-key pin set")
	}

	runner := sshtrust.ExecRunner{}
	probes := make(map[string]nodeProbe, len(hosts))
	for _, host := range hosts {
		args := []string{
			"-F", "/dev/null",
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile=" + known,
			"-o", "GlobalKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=5",
			"-o", "ConnectionAttempts=1",
			"-o", "IdentitiesOnly=yes",
			"-o", "IdentityAgent=none",
			"-o", "BatchMode=no",
			"-o", "PreferredAuthentications=password",
			"-o", "PubkeyAuthentication=no",
			"-o", "NumberOfPasswordPrompts=1",
			"-p", strconv.Itoa(host.Node.SSHPort),
			host.Node.SSHUser + "@" + host.Node.ManagementIP,
			"python3 -c " + shellQuoteAutoQuery(readOnlyProbePython),
		}
		raw, err := runner.Run(ctx, nil, password, "ssh", args...)
		if err != nil {
			return nil, fmt.Errorf("read-only query failed on %s (check SSH password and prerequisites)", host.Node.Name)
		}
		var probe nodeProbe
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("invalid read-only query response from %s", host.Node.Name)
		}
		probe.Node = host.Node.Name
		probes[host.Node.Name] = probe
	}
	return probes, nil
}

func shellQuoteAutoQuery(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func applyAutoQuery(plan *model.RealmPlan, probes map[string]nodeProbe, profile discoveryProfile) ([]string, []string) {
	var findings, warnings []string
	if plan.Installation == nil {
		return findings, []string{"installation settings are missing"}
	}
	storage := make([]model.NodeSpec, 0)
	for _, node := range plan.Nodes {
		if probes[node.Name].Existing {
			warnings = append(warnings, node.Name+": existing Titanus/Ceph configuration or active service detected; inspect it before attempting a fresh installation")
		}
		if hasCapability(node, model.CapabilityStorage) {
			storage = append(storage, node)
		}
	}
	if plan.Ceph.Enabled && len(storage) > 0 {
		if cidr := commonNodeCIDR(storage, probes, func(n model.NodeSpec) string {
			if n.CephPublicIP != "" {
				return n.CephPublicIP
			}
			return n.ManagementIP
		}); cidr != "" {
			if plan.Ceph.PublicCIDR != "" && plan.Ceph.PublicCIDR != cidr {
				warnings = append(warnings, "Ceph public network changed from "+plan.Ceph.PublicCIDR+" to detected "+cidr+"; review before validation")
			}
			plan.Ceph.PublicCIDR = cidr
			findings = append(findings, "Ceph public network auto-detected as "+cidr)
		} else {
			warnings = append(warnings, "No common Ceph public network could be proven; existing value preserved")
		}
		if cidr := commonNodeCIDR(storage, probes, func(n model.NodeSpec) string {
			if n.CephClusterIP != "" {
				return n.CephClusterIP
			}
			return n.ManagementIP
		}); cidr != "" {
			if plan.Ceph.ClusterCIDR != "" && plan.Ceph.ClusterCIDR != cidr {
				warnings = append(warnings, "Ceph cluster network changed from "+plan.Ceph.ClusterCIDR+" to detected "+cidr+"; review before validation")
			}
			plan.Ceph.ClusterCIDR = cidr
			findings = append(findings, "Ceph cluster network auto-detected as "+cidr)
		} else {
			warnings = append(warnings, "No common Ceph cluster network could be proven; existing value preserved")
		}
		if plan.Installation.Profile == model.ProfileNativeHA {
			plan.Ceph.Replication = 3
		}
		var raw uint64
		for idx := range plan.Nodes {
			node := &plan.Nodes[idx]
			if !hasCapability(*node, model.CapabilityStorage) {
				continue
			}
			probe, ok := probes[node.Name]
			if !ok {
				warnings = append(warnings, "No storage probe result for "+node.Name)
				continue
			}
			if len(probe.Devices) == 0 {
				warnings = append(warnings, "No signature-free, unmounted whole data disks found on "+node.Name)
				continue
			}
			if len(node.CephDevices) > 0 {
				available := map[string]uint64{}
				for _, d := range probe.Devices {
					available[d.Path] = d.Size
				}
				safe := true
				for _, selected := range node.CephDevices {
					size, ok := available[selected]
					if !ok {
						safe = false
					} else {
						raw += size
					}
				}
				if safe {
					findings = append(findings, node.Name+": existing Ceph device selection re-verified unused")
				} else {
					warnings = append(warnings, node.Name+": existing Ceph device selection includes a device not proven unused; inspect it before validation")
				}
				continue
			}
			node.CephDevices = node.CephDevices[:0]
			for _, device := range probe.Devices {
				node.CephDevices = append(node.CephDevices, device.Path)
				raw += device.Size
			}
			findings = append(findings, fmt.Sprintf("%s: selected %d read-only verified unused Ceph device(s)", node.Name, len(probe.Devices)))
		}
		if plan.Ceph.RequestedTB <= 0 && raw > 0 {
			replication := plan.Ceph.Replication
			if replication < 1 {
				replication = 1
			}
			usable := float64(raw) / float64(replication) / 1e12 * 0.90
			if usable >= 1 {
				plan.Ceph.RequestedTB = int(usable)
				findings = append(findings, fmt.Sprintf("Ceph requested usable capacity set conservatively to %d TB", plan.Ceph.RequestedTB))
			}
		}
	}

	if plan.Installation.Profile == model.ProfileNativeHA {
		if plan.Installation.PowerFencing == nil {
			plan.Installation.PowerFencing = map[string]fencing.Target{}
		}
		for _, node := range plan.Nodes {
			if !hasCapability(node, model.CapabilityExecution) {
				continue
			}
			probe := probes[node.Name]
			target, exists := plan.Installation.PowerFencing[node.Name]
			if !exists {
				target, exists = profile.PowerFencing[node.Name]
			}
			if probe.UUID != "" {
				if target.UUID != "" && !strings.EqualFold(target.UUID, probe.UUID) {
					warnings = append(warnings, node.Name+": discovered UUID conflicts with the configured fencing identity; existing target preserved")
					continue
				}
				target.UUID = strings.ToLower(probe.UUID)
			}
			if exists || target.Provider != "" {
				plan.Installation.PowerFencing[node.Name] = target
				if err := fencing.ValidateTargets(map[string]fencing.Target{node.Name: target}, true); err != nil {
					warnings = append(warnings, fmt.Sprintf("%s fencing needs attention: %v", node.Name, err))
				} else {
					findings = append(findings, node.Name+": fencing identity and private credential files verified")

				}
			} else {
				warnings = append(warnings, node.Name+": UUID discovered, but no server-side fencing provider profile is configured")
			}
		}
	}
	return findings, warnings
}

func hasCapability(node model.NodeSpec, wanted model.Capability) bool {
	for _, capability := range node.Capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func commonNodeCIDR(nodes []model.NodeSpec, probes map[string]nodeProbe, selected func(model.NodeSpec) string) string {
	common := ""
	for _, node := range nodes {
		ip := net.ParseIP(selected(node))
		if ip == nil {
			return ""
		}
		found := ""
		for _, value := range probes[node.Name].CIDRs {
			_, network, err := net.ParseCIDR(value)
			if err == nil && network.Contains(ip) {
				found = network.String()
				break
			}
		}
		if found == "" {
			return ""
		}
		if common == "" {
			common = found
		} else if common != found {
			return ""
		}
	}
	return common
}

func loadDiscoveryProfile() (discoveryProfile, []string) {
	var profile discoveryProfile
	data, err := fencing.ReadPrivateFile(discoveryProfilePath)
	if os.IsNotExist(err) {
		return profile, nil
	}
	if err != nil {
		return profile, []string{"Server-side discovery profile ignored: " + err.Error()}
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return discoveryProfile{}, []string{"Server-side discovery profile ignored: invalid JSON"}
	}
	if decoder.Decode(new(any)) != io.EOF {
		return discoveryProfile{}, []string{"Server-side discovery profile ignored: trailing JSON"}
	}
	return profile, nil
}

func autoFillArchives(plan *model.RealmPlan, profile discoveryProfile) ([]string, []string) {
	if plan.Installation == nil || plan.Installation.Operation == "rollback" {
		return nil, nil
	}
	if plan.Installation.Archives == nil {
		plan.Installation.Archives = map[string]model.Archive{}
	}
	dirs := append([]string{}, profile.ReleaseDirs...)
	dirs = append(dirs,
		releasecache.Directory(plan.Installation.ReleaseVersion, plan.Installation.Revision),
		"/root/titanus-web-candidate",
		filepath.Join("/var/lib/titanus/installer-web", "releases"),
		"/var/lib/titanus/releases",
	)
	var findings, warnings []string
	for _, arch := range []string{"amd64", "arm64"} {
		existing := plan.Installation.Archives[arch]
		if existing.Path != "" {
			sum, err := verifiedArchive(existing.Path, existing.SHA256, plan.Installation.ReleaseVersion, plan.Installation.Revision, arch)
			if err != nil {
				warnings = append(warnings, strings.ToUpper(arch)+" selected release archive failed verification; path and pinned checksum preserved for review")
			} else {
				existing.SHA256 = sum
				plan.Installation.Archives[arch] = existing
				findings = append(findings, strings.ToUpper(arch)+" release manifest, executable inventory and checksum verified")
			}
			continue
		}
		name := fmt.Sprintf("titanus-%s-linux-%s.tar.gz", plan.Installation.ReleaseVersion, arch)
		for _, dir := range uniqueStrings(dirs) {
			path := filepath.Join(filepath.Clean(dir), name)
			if !archiveMatches(path, plan.Installation.ReleaseVersion, plan.Installation.Revision, arch) {
				continue
			}
			sum, err := verifiedArchive(path, "", plan.Installation.ReleaseVersion, plan.Installation.Revision, arch)
			if err != nil {
				continue
			}
			plan.Installation.Archives[arch] = model.Archive{Path: path, SHA256: sum}
			findings = append(findings, strings.ToUpper(arch)+" matching release archive inventory verified and SHA256 calculated")
			break
		}
		if plan.Installation.Archives[arch].Path == "" {
			warnings = append(warnings, strings.ToUpper(arch)+" matching verified release archive was not found on the installer host")
		}
	}
	return findings, warnings
}

func verifiedArchive(path, pinned, version, revision, arch string) (string, error) {
	if !archiveMatches(path, version, revision, arch) {
		return "", fmt.Errorf("release identity mismatch")
	}
	sum, err := fileSHA256(path)
	if err != nil {
		return "", err
	}
	if pinned != "" && pinned != sum {
		return "", fmt.Errorf("pinned checksum mismatch")
	}
	if _, err := release.Verify(path, sum, version, revision, arch); err != nil {
		return "", err
	}
	return sum, nil
}

func archiveMatches(path, version, revision, arch string) bool {
	if path == "" {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return false
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return false
		}
		if filepath.Base(header.Name) != "manifest.json" || header.Size > 1<<20 {
			continue
		}
		var manifest struct {
			Version  string `json:"version"`
			Revision string `json:"revision"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
		}
		if json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&manifest) != nil {
			return false
		}
		return manifest.Version == version && manifest.Revision == revision && manifest.OS == "linux" && manifest.Arch == arch
	}
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
