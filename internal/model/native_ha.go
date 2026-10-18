package model

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"path/filepath"
)

func (p RealmPlan) validateNativeHA() error {
	if p.Installation == nil || p.Installation.Profile != ProfileNativeHA {
		return nil
	}
	controls, workers := 0, 0
	targets := p.Installation.PowerFencing
	for _, n := range p.Nodes {
		control, worker := false, false
		for _, c := range n.Capabilities {
			control = control || c == CapabilityControl
			worker = worker || c == CapabilityExecution
		}
		if control {
			controls++
		}
		if control && worker {
			return fmt.Errorf("Native HA separates CONTROL voters from EXECUTION Workers to preserve quorum during fencing")
		}
		if worker {
			workers++
			if _, ok := targets[n.Name]; !ok {
				return fmt.Errorf("Native HA Worker %s has no bound power fencing target", n.Name)
			}
		}
	}
	if (controls != 3 && controls != 5) || workers < 2 {
		return fmt.Errorf("Native HA requires 3 or 5 CONTROL voters and at least 2 separate EXECUTION Workers (5–9 hosts)")
	}
	known := map[string]bool{}
	for _, n := range p.Nodes {
		known[n.Name] = true
	}
	for name, t := range targets {
		if !known[name] {
			return fmt.Errorf("unknown power fencing target %s", name)
		}
		for _, path := range []string{t.CredentialFile, t.CAFile} {
			if path != "" && (!filepath.IsAbs(path) || filepath.Clean(path) != path) {
				return fmt.Errorf("fencing file paths must be absolute and clean")
			}
		}
	}
	return fencing.ValidateTargets(targets, false)
}
