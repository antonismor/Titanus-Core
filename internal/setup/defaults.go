package setup

import (
	"fmt"
	"net"
	"net/netip"
	"sort"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func defaultNodeCapabilities(profile model.InstallationProfile, index, count, controls, storage int) []model.Capability {
	caps := []model.Capability{model.CapabilityExecution}
	if index < controls {
		caps = []model.Capability{model.CapabilityControl, model.CapabilityExecution}
		if profile == model.ProfileNativeHA {
			caps = caps[:1]
		}
	}
	// A dedicated storage tier leaves at least two separate Native HA Workers.
	storageStart := 0
	if profile == model.ProfileNativeHA && count-controls-storage >= 2 {
		storageStart = count - storage
		if index >= storageStart {
			caps = nil
		}
	}
	if storage > 0 && index >= storageStart && index < storageStart+storage {
		caps = append(caps, model.CapabilityStorage)
	}
	return caps
}

func defaultNodeName(caps []model.Capability, previous []model.NodeSpec) string {
	role := func(c []model.Capability) string {
		n := model.NodeSpec{Capabilities: c}
		if hasCapability(n, model.CapabilityControl) {
			return "ctl"
		}
		if hasCapability(n, model.CapabilityStorage) && !hasCapability(n, model.CapabilityExecution) {
			return "ceph"
		}
		return "wrk"
	}
	label, ordinal := role(caps), 1
	for _, n := range previous {
		if role(n.Capabilities) == label {
			ordinal++
		}
	}
	return fmt.Sprintf("titanus-%s%02d", label, ordinal)
}

func managementAddress(base string, offset, prefix int) string {
	ip, err := netip.ParseAddr(base)
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || offset < 0 || prefix < 1 || prefix > 32 {
		return ""
	}
	network := netip.PrefixFrom(ip, prefix).Masked()
	for i := 0; i < offset; i++ {
		ip = ip.Next()
		if !ip.IsValid() || !network.Contains(ip) {
			return ""
		}
	}
	if prefix <= 30 && (ip == network.Addr() || !network.Contains(ip.Next())) {
		return ""
	}
	return ip.String()
}

func localManagementAddress() string {
	addresses, _ := net.InterfaceAddrs()
	var candidates []string
	for _, a := range addresses {
		prefix, err := netip.ParsePrefix(a.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsGlobalUnicast() {
			candidates = append(candidates, prefix.Addr().String())
		}
	}
	sort.Strings(candidates)
	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func defaultCephCIDR(nodes []model.NodeSpec, cluster bool, bits int, fallback string) string {
	var common netip.Prefix
	for _, node := range nodes {
		if !hasCapability(node, model.CapabilityStorage) {
			continue
		}
		address := node.CephPublicIP
		if cluster {
			address = node.CephClusterIP
		}
		ip, err := netip.ParseAddr(address)
		if err != nil || !ip.Is4() {
			return fallback
		}
		prefix := netip.PrefixFrom(ip, bits).Masked()
		if common.IsValid() && common != prefix {
			return fallback
		}
		common = prefix
	}
	if common.IsValid() {
		return common.String()
	}
	return fallback
}
