package deploy

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
)

// All credential bytes are in the private, checksum-bound configuration archive,
// never in the plan, generated shell, logs, or non-controller packages.
func NativeHAFencingConfig(i model.Installation) (map[string][]byte, error) {
	cfg := map[string][]byte{}
	targets := map[string]fencing.Target{}
	for id, t := range i.PowerFencing {
		if t.CredentialFile != "" {
			b, e := fencing.ReadPrivateFile(t.CredentialFile)
			if e != nil {
				return nil, fmt.Errorf("Worker %s fencing credential: %w", id, e)
			}
			name := "fencing/" + id + ".credential"
			cfg[name] = b
			t.CredentialFile = "/etc/titanus/" + name
		}
		if t.CAFile != "" {
			b, e := fencing.ReadPrivateFile(t.CAFile)
			if e != nil {
				return nil, fmt.Errorf("Worker %s fencing CA: %w", id, e)
			}
			name := "fencing/" + id + ".ca"
			cfg[name] = b
			t.CAFile = "/etc/titanus/" + name
		}
		targets[id] = t
	}
	b, e := json.Marshal(targets)
	if e != nil {
		return nil, e
	}
	cfg["power-fencing.json"] = b
	return cfg, nil
}
