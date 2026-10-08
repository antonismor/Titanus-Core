package disk

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// A short-lived native librados client holds a non-expiring distributed lock
// while mapping or maintaining an RBD. This closes the unmap/snapshot/map race.
// A killed guard leaves its lock in RADOS; no timer can authorize new writers.
// Recovery must inspect and explicitly fence/clear that abandoned guard.
const radosGuardProgram = `
import sys,json,rados,uuid
cfg=json.loads(sys.argv[1])
cluster=rados.Rados(conffile=cfg['conf'],name=cfg['client'],clustername=cfg['cluster'])
if cfg.get('keyring'): cluster.conf_set('keyring',cfg['keyring'])
cluster.connect()
io=cluster.open_ioctx(cfg['rbd_pool'])
cookie=str(uuid.uuid4())
obj='titanus.lifecycle.'+sys.argv[2]
io.lock_exclusive(obj,'lifecycle',cookie,'Titanus RBD lifecycle')
try:
 print('READY',flush=True)
 message=sys.stdin.buffer.readline()
 if message==b'RELEASE\n': io.unlock(obj,'lifecycle',cookie)
finally:
 io.close()
 cluster.shutdown()
`

func (m *Manager) radosGuard(name string) (func(), error) {
	if !diskName.MatchString(name) {
		return nil, fmt.Errorf("invalid Disk name")
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	cmd := exec.CommandContext(ctx, "python3", "-u", "-c", radosGuardProgram, string(raw), name)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		stdin.Close()
		cancel()
		return nil, err
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "READY" {
		stdin.Close()
		waitErr := cmd.Wait()
		cancel()
		return nil, fmt.Errorf("RADOS lifecycle guard unavailable: %w: %s", waitErr, stderr.String())
	}
	var once sync.Once
	return func() { once.Do(func() { stdin.Close(); _ = cmd.Wait(); cancel() }) }, nil
}
func (m *Manager) guardSpec(spec Spec) (func(), error) {
	if spec.Provider == ProviderCephRBD {
		return m.radosGuard(spec.Name)
	}
	return func() {}, nil
}
