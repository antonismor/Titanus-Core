package sshtrust

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/model"
)

// ReadOnlyUUIDs does not install keys, alter known_hosts or start any service.
// Existing pinned keys remain mandatory. The discovered key is bound to the
// password-authenticated inspection in a private temporary known_hosts file.
func (b Bootstrap) ReadOnlyUUIDs(ctx context.Context, nodes []model.NodeSpec, password []byte) (map[string]string, error) {
	if err := CheckTools(); err != nil {
		return nil, err
	}
	if len(password) == 0 || len(password) > 4096 || strings.ContainsAny(string(password), "\x00\r\n") {
		return nil, fmt.Errorf("initial SSH password required")
	}
	hosts, err := b.Discover(ctx, nodes)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "titanus-uuid-query-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	known := filepath.Join(dir, "known_hosts")
	var lines strings.Builder
	for _, h := range hosts {
		fmt.Fprintf(&lines, "%s %s\n", hostLabel(h.Node), h.Key)
	}
	if err = os.WriteFile(known, []byte(lines.String()), 0600); err != nil {
		return nil, err
	}
	results := map[string]string{}
	for _, h := range hosts {
		args := []string{"-F", "/dev/null", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + known, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "ConnectTimeout=5", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=no", "-o", "PreferredAuthentications=password", "-o", "PubkeyAuthentication=no", "-o", "NumberOfPasswordPrompts=1", "-p", strconv.Itoa(h.Node.SSHPort), h.Node.SSHUser + "@" + h.Node.ManagementIP, "cat /sys/class/dmi/id/product_uuid"}
		raw, err := b.Runner.Run(ctx, nil, password, "ssh", args...)
		if err != nil {
			return nil, fmt.Errorf("read-only UUID query failed on %s", h.Node.Name)
		}
		uuid := strings.ToLower(strings.TrimSpace(string(raw)))
		if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`).MatchString(uuid) {
			return nil, fmt.Errorf("invalid guest UUID on %s", h.Node.Name)
		}
		results[h.Node.Name] = uuid
	}
	return results, nil
}
