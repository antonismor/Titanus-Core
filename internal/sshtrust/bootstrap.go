package sshtrust

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/antonismor/Titanus-Core/internal/model"
)

type Host struct {
	Node             model.NodeSpec
	Key, Fingerprint string
}
type Report struct {
	Verified, Expected           int
	IdentityFile, KnownHostsFile string
}
type Bootstrap struct {
	Runner                        Runner
	Directory, ExistingKnownHosts string
}

func RealmID(name string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(name)))[:24] }
func hostLabel(n model.NodeSpec) string {
	if n.SSHPort == 22 {
		return n.ManagementIP
	}
	return "[" + n.ManagementIP + "]:" + strconv.Itoa(n.SSHPort)
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func parseKey(raw []byte) (string, error) {
	fields := strings.Fields(string(raw))
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", fmt.Errorf("expected an Ed25519 public key")
	}
	data, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(data) != 51 || string(data[4:15]) != "ssh-ed25519" || data[0] != 0 || data[1] != 0 || data[2] != 0 || data[3] != 11 || data[15] != 0 || data[16] != 0 || data[17] != 0 || data[18] != 32 {
		return "", fmt.Errorf("invalid Ed25519 public key")
	}
	return fields[0] + " " + fields[1], nil
}
func fingerprint(key string) string {
	fields := strings.Fields(key)
	b, _ := base64.StdEncoding.DecodeString(fields[1])
	s := sha256.Sum256(b)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(s[:])
}
func validateNodes(nodes []model.NodeSpec) error {
	if len(nodes) < 1 || len(nodes) > model.CommunityNodeLimit {
		return fmt.Errorf("SSH bootstrap requires 1–9 nodes")
	}
	seen := map[string]bool{}
	for _, n := range nodes {
		ip := net.ParseIP(n.ManagementIP)
		if n.Name == "" || ip == nil || ip.IsUnspecified() || ip.IsMulticast() || n.SSHPort < 1 || n.SSHPort > 65535 || n.SSHUser == "" || strings.HasPrefix(n.SSHUser, "-") || strings.ContainsAny(n.SSHUser, " \t\r\n\x00@/:") {
			return fmt.Errorf("invalid SSH node inventory")
		}
		// One host cannot evade full-mesh checks through aliases or multiple ports.
		if seen[ip.String()] || seen["name:"+n.Name] {
			return fmt.Errorf("duplicate SSH node")
		}
		seen[ip.String()] = true
		seen["name:"+n.Name] = true
	}
	return nil
}

// Discover performs no authentication and makes no remote changes. Changed
// pinned Ed25519 host keys are fatal, including on a resumed bootstrap.
func (b Bootstrap) Discover(ctx context.Context, nodes []model.NodeSpec) ([]Host, error) {
	if err := validateNodes(nodes); err != nil {
		return nil, err
	}
	hosts := make([]Host, 0, len(nodes))
	for _, n := range nodes {
		raw, err := b.Runner.Run(ctx, nil, nil, "ssh-keyscan", "-T", "5", "-t", "ed25519", "-p", strconv.Itoa(n.SSHPort), n.ManagementIP)
		if err != nil {
			return nil, fmt.Errorf("host key scan failed for %s", n.Name)
		}
		var key string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue
			}
			f := strings.Fields(line)
			if len(f) != 3 {
				return nil, fmt.Errorf("invalid host key scan for %s", n.Name)
			}
			k, e := parseKey([]byte(strings.Join(f[1:], " ")))
			if e != nil {
				return nil, e
			}
			if key != "" && key != k {
				return nil, fmt.Errorf("ambiguous host key for %s", n.Name)
			}
			key = k
		}
		if key == "" {
			return nil, fmt.Errorf("missing host key for %s", n.Name)
		}
		for _, file := range []string{b.ExistingKnownHosts, filepath.Join(b.Directory, "known_hosts")} {
			if file == "" {
				continue
			}
			if _, e := os.Stat(file); os.IsNotExist(e) {
				continue
			} else if e != nil {
				return nil, e
			}
			// ssh-keygen handles hashed known_hosts entries as well as plain entries.
			prior, e := b.Runner.Run(ctx, nil, nil, "sh", "-c", `ssh-keygen -F "$1" -f "$2"; code=$?; if [ "$code" = 1 ]; then exit 0; fi; exit "$code"`, "titanus-known-host", hostLabel(n), file)
			if e != nil {
				return nil, fmt.Errorf("cannot inspect existing host pins for %s", n.Name)
			}
			for _, line := range strings.Split(string(prior), "\n") {
				f := strings.Fields(line)
				if len(f) < 3 || strings.HasPrefix(f[0], "#") {
					continue
				}
				if f[0] == "@revoked" {
					return nil, fmt.Errorf("revoked host key for %s", n.Name)
				}
				if f[1] == "ssh-ed25519" && strings.Join(f[1:3], " ") != key {
					return nil, fmt.Errorf("host key changed for %s; verify and resolve the existing pin manually", n.Name)
				}
			}
		}
		hosts = append(hosts, Host{Node: n, Key: key, Fingerprint: fingerprint(key)})
	}
	return hosts, nil
}
func secureDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	// Reject symlinked or group/world-accessible paths all the way to HOME.
	for p := filepath.Clean(path); p != "." && p != string(filepath.Separator); p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("unsafe SSH trust directory")
		}
		if p == path && (info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
			return fmt.Errorf("SSH trust directory must have mode 0700")
		}
	}
	return nil
}
func safeFile(path string) error {
	i, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !i.Mode().IsRegular() || i.Mode().Perm()&0077 != 0 || i.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return fmt.Errorf("unsafe SSH trust file permissions")
	}
	return nil
}
func (b Bootstrap) ssh(ctx context.Context, h Host, password []byte, command string) ([]byte, error) {
	args := []string{"-F", "/dev/null", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + filepath.Join(b.Directory, "known_hosts"), "-o", "GlobalKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-i", filepath.Join(b.Directory, "id_ed25519"), "-p", strconv.Itoa(h.Node.SSHPort)}
	if len(password) > 0 {
		args = append(args, "-o", "BatchMode=no", "-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no", "-o", "NumberOfPasswordPrompts=1")
	} else {
		args = append(args, "-o", "BatchMode=yes")
	}
	args = append(args, h.Node.SSHUser+"@"+h.Node.ManagementIP, command)
	return b.Runner.Run(ctx, nil, password, "ssh", args...)
}

// Apply requires the caller to approve every discovered fingerprint first.
// Every private node key is generated on its own host and never downloaded.
func (b Bootstrap) Apply(ctx context.Context, realm string, hosts []Host, password []byte) (Report, error) {
	report := Report{Expected: len(hosts) * (len(hosts) - 1), IdentityFile: filepath.Join(b.Directory, "id_ed25519"), KnownHostsFile: filepath.Join(b.Directory, "known_hosts")}
	nodes := make([]model.NodeSpec, len(hosts))
	for i, h := range hosts {
		nodes[i] = h.Node
		if k, e := parseKey([]byte(h.Key)); e != nil || k != h.Key || fingerprint(k) != h.Fingerprint {
			return report, fmt.Errorf("invalid approved host key")
		}
	}
	if err := validateNodes(nodes); err != nil {
		return report, err
	}
	if len(password) == 0 || len(password) > 4096 || strings.ContainsAny(string(password), "\x00\r\n") {
		return report, fmt.Errorf("a nonempty single-line bootstrap password is required (maximum 4096 bytes)")
	}
	if err := secureDir(b.Directory); err != nil {
		return report, err
	}
	var known strings.Builder
	for _, h := range hosts {
		fmt.Fprintln(&known, hostLabel(h.Node), h.Key)
	}
	if _, err := os.Lstat(report.KnownHostsFile); err == nil {
		if err = safeFile(report.KnownHostsFile); err != nil {
			return report, err
		}
	}
	if err := os.WriteFile(report.KnownHostsFile, []byte(known.String()), 0600); err != nil {
		return report, err
	}
	if _, err := os.Lstat(report.IdentityFile); os.IsNotExist(err) {
		if _, err = b.Runner.Run(ctx, nil, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", report.IdentityFile); err != nil {
			return report, err
		}
	} else if err != nil {
		return report, err
	}
	if err := safeFile(report.IdentityFile); err != nil {
		return report, err
	}
	// Derive the public key from the private key so a stale .pub cannot be trusted.
	public, err := b.Runner.Run(ctx, nil, nil, "ssh-keygen", "-y", "-P", "", "-f", report.IdentityFile)
	if err != nil {
		return report, err
	}
	installerKey, err := parseKey(public)
	if err != nil {
		return report, err
	}
	id := RealmID(realm)
	keys := make([]string, len(hosts))
	for i, h := range hosts {
		raw, e := b.ssh(ctx, h, password, remoteCommand(id, installerKey, nil, ""))
		if e != nil {
			return report, fmt.Errorf("key preparation failed on %s", h.Node.Name)
		}
		keys[i], err = parseKey(raw)
		if err != nil {
			return report, err
		}
		if keys[i] == installerKey {
			return report, fmt.Errorf("installer and node keys must be distinct")
		}
		for j := 0; j < i; j++ {
			if keys[j] == keys[i] {
				return report, fmt.Errorf("node keys must be distinct; cloned key detected on %s", h.Node.Name)
			}
		}
	}
	for _, h := range hosts {
		if _, err = b.ssh(ctx, h, nil, remoteCommand(id, installerKey, keys, known.String())); err != nil {
			return report, fmt.Errorf("trust distribution failed on %s", h.Node.Name)
		}
	}
	// All ordered pairs are tested, including reverse directions. One node has
	// zero cross-node pairs; installer-to-node key authentication still ran above.
	for _, source := range hosts {
		for _, target := range hosts {
			if source.Node.Name == target.Node.Name {
				continue
			}
			cmd := meshCommand(id, target.Node)
			raw, e := b.ssh(ctx, source, nil, cmd)
			if e != nil || strings.TrimSpace(string(raw)) != "TITANUS_MESH_OK" {
				return report, fmt.Errorf("full-mesh verification failed: %s -> %s (%d/%d verified)", source.Node.Name, target.Node.Name, report.Verified, report.Expected)
			}
			report.Verified++
		}
	}
	return report, nil
}
func remoteCommand(id, installer string, peers []string, known string) string {
	payload, _ := json.Marshal(map[string]any{"id": id, "installer": installer, "peers": peers, "known": known})
	return "python3 -c " + quote(remotePython) + " " + quote(string(payload))
}
func meshCommand(id string, target model.NodeSpec) string {
	base := "$HOME/.ssh/titanus/" + id
	return "ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile=\"" + base + "/known_hosts\" -o IdentitiesOnly=yes -o IdentityAgent=none -o ConnectTimeout=5 -o ConnectionAttempts=1 -i \"" + base + "/id_ed25519\" -p " + strconv.Itoa(target.SSHPort) + " " + quote(target.SSHUser+"@"+target.ManagementIP) + " 'printf TITANUS_MESH_OK'"
}
