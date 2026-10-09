package model

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

type Archive struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type Installation struct {
	Operation      string             `json:"operation"` // install, upgrade, rollback
	ReleaseVersion string             `json:"release_version"`
	Revision       string             `json:"revision"`
	Archives       map[string]Archive `json:"archives,omitempty"`
	PKIDir         string             `json:"pki_dir,omitempty"`
	APIPort        int                `json:"api_port"`
	RaftPort       int                `json:"raft_port"`
	NodePrefix     int                `json:"node_prefix"`
	VXLANID        int                `json:"vxlan_id"`
	Start          bool               `json:"start"`
}

func (i Installation) Validate() error {
	if i.Operation != "install" && i.Operation != "upgrade" && i.Operation != "rollback" {
		return fmt.Errorf("installation operation must be install, upgrade or rollback")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?$`).MatchString(i.ReleaseVersion) || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(i.Revision) {
		return fmt.Errorf("exact release version and 40-character tested main revision required")
	}
	if i.Operation != "rollback" {
		if len(i.Archives) != 2 {
			return fmt.Errorf("both native archives required")
		}
		for _, arch := range []string{"amd64", "arm64"} {
			a := i.Archives[arch]
			if a.Path == "" || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(a.SHA256) {
				return fmt.Errorf("%s archive and pinned SHA256 required", arch)
			}
		}
	}
	if i.APIPort < 1 || i.APIPort > 65535 || i.RaftPort < 1 || i.RaftPort > 65535 || i.APIPort == i.RaftPort || i.NodePrefix < 16 || i.NodePrefix > 30 || i.VXLANID < 1 || i.VXLANID > 16777215 {
		return fmt.Errorf("invalid installation network parameters")
	}
	return nil
}

func (p RealmPlan) validateInstallation() error {
	if p.Installation == nil {
		if p.Version == "titanus-plan/v2" {
			return fmt.Errorf("plan v2 requires installation settings")
		}
		return nil
	}
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(p.RealmName) {
		return fmt.Errorf("Realm name must be a safe identity and directory name")
	}
	_, fabric, _ := net.ParseCIDR(p.FabricCIDR)
	_, services, _ := net.ParseCIDR(p.ServiceCIDR)
	if fabric == nil || services == nil {
		return fmt.Errorf("valid Fabric and Service CIDRs required")
	}
	ones, bits := fabric.Mask.Size()
	serviceOnes, serviceBits := services.Mask.Size()
	if bits != 32 || serviceBits != 32 || p.Installation.NodePrefix <= ones || serviceOnes > 30 || fabric.Contains(services.IP) || services.Contains(fabric.IP) {
		return fmt.Errorf("non-overlapping IPv4 Fabric/Service networks and compatible node prefix required")
	}
	if uint64(len(p.Nodes)) > uint64(1)<<uint(p.Installation.NodePrefix-ones) {
		return fmt.Errorf("Fabric has insufficient per-node subnets")
	}
	for _, n := range p.Nodes {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(n.Name) || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`).MatchString(n.SSHUser) {
			return fmt.Errorf("safe node and SSH user names required")
		}
		if len(n.Capabilities) == 0 {
			return fmt.Errorf("node %s needs capabilities", n.Name)
		}
		seen := map[Capability]bool{}
		for _, c := range n.Capabilities {
			parsed, err := ParseCapabilities(string(c))
			if err != nil || len(parsed) != 1 || parsed[0] != c || seen[c] {
				return fmt.Errorf("node %s has unknown or duplicate capabilities", n.Name)
			}
			seen[c] = true
		}
		for _, address := range []string{n.ManagementIP, n.FabricIP, n.CephPublicIP, n.CephClusterIP} {
			if address == "" {
				continue
			}
			ip := net.ParseIP(address)
			if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || fabric.Contains(ip) || services.Contains(ip) {
				return fmt.Errorf("node %s host addresses must be IPv4 outside Unit/Service networks", n.Name)
			}
		}
	}
	if p.Installation.Operation != "install" && p.Ceph.Provision {
		return fmt.Errorf("Ceph provisioning is restricted to initial installation")
	}
	if p.Ceph.Provision && !p.Installation.Start {
		return fmt.Errorf("Ceph provisioning requires starting the installed Realm")
	}
	if strings.ContainsAny(p.Installation.PKIDir, "\x00\r\n") {
		return fmt.Errorf("invalid PKI directory")
	}
	return nil
}
