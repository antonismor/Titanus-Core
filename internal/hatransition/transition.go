// Package hatransition prepares a bounded, non-destructive profile transition.
// It preserves the existing three/five-voter consensus membership and all data.
package hatransition

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	"strconv"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/backup"
	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/haprofile"

	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
)

type Prepared = haprofile.Prepared

var Digest = haprofile.Digest
var ValidateTarget = haprofile.ValidateTarget
var Capabilities = haprofile.Capabilities

func Prepare(p model.RealmPlan, setPath, keyPath, output string) (Prepared, error) {
	var out Prepared
	set, e := backup.VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return out, e
	}
	if e = ValidateTarget(set.Intent.State, p); e != nil {
		return out, e
	}
	controls := map[string]model.NodeSpec{}
	for _, n := range p.ControlNodes() {
		controls[n.Name] = n
	}
	if len(set.Intent.Binding.Peers) != len(controls) {
		return out, fmt.Errorf("unchanged three/five-voter backup membership required; standalone conversion is unsupported")
	}
	for _, peer := range set.Intent.Binding.Peers {
		n, ok := controls[peer.ID]
		if !ok || peer.Address != net.JoinHostPort(n.ManagementIP, strconv.Itoa(p.Installation.RaftPort)) || peer.API != "https://"+net.JoinHostPort(n.ManagementIP, strconv.Itoa(p.Installation.APIPort)) {
			return out, fmt.Errorf("target differs from backed-up controller membership")
		}
	}
	if len(set.Hosts) != len(p.Nodes) {
		return out, fmt.Errorf("complete host recovery set required")
	}
	cfg, e := deploy.NativeHAFencingConfig(*p.Installation)
	if e != nil {
		return Prepared{}, e
	}
	hashes := map[string]string{}
	for name, data := range cfg {
		hashes[name] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	out = Prepared{Format: "titanus-ha-transition/v1", Plan: p, BackupSetID: set.Intent.Binding.ID, BackupRevision: set.Intent.Binding.Revision, BackupStateSHA256: set.Intent.Binding.RealmSHA256, Hosts: hostPlans(set.Intent.Hosts), FencingSHA256: hashes}
	out.ID = Digest(out)
	if _, e = os.Lstat(output); !os.IsNotExist(e) {
		return Prepared{}, fmt.Errorf("new private transition output directory required")
	}
	if e = verifyParents(filepath.Join(output, "transition.json")); e != nil {
		return Prepared{}, e
	}
	if e = os.MkdirAll(output, 0700); e != nil {
		return Prepared{}, e
	}
	// Reuse the exact controller-only private fencing packaging path.
	for name, data := range cfg {
		path := filepath.Join(output, "fencing", name)
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return Prepared{}, e
		}
		if e = os.WriteFile(path, data, 0600); e != nil {
			return Prepared{}, e
		}
	}
	return out, durable.WriteJSON(filepath.Join(output, "transition.json"), out, 0600)
}
func Load(path string) (Prepared, error) {
	var p Prepared
	b, e := fencing.ReadPrivateFile(path)
	if e != nil {
		return p, e
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if e = d.Decode(&p); e != nil {
		return p, e
	}
	if e = d.Decode(new(any)); e != io.EOF {
		return p, fmt.Errorf("unexpected trailing transition content")
	}
	return p, p.Validate()
}

func hostPlans(hosts []backup.Plan) []haprofile.HostPlan {
	out := make([]haprofile.HostPlan, len(hosts))
	for i, h := range hosts {
		out[i] = haprofile.HostPlan(h)
	}
	return out
}
