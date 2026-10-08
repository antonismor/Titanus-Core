package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"time"
)

// GuardedNodes proves current quorum leadership immediately before each remote
// action. This is scheduling authority, not a substitute for storage fencing.
type GuardedNodes struct {
	NodeRuntime
	Check func() error
}

func (g *GuardedNodes) EnsureUnit(a string, s unitruntime.Spec) (unitruntime.State, error) {
	if e := g.Check(); e != nil {
		return unitruntime.State{}, e
	}
	return g.NodeRuntime.EnsureUnit(a, s)
}
func (g *GuardedNodes) StartUnit(a, id string) (unitruntime.State, error) {
	if e := g.Check(); e != nil {
		return unitruntime.State{}, e
	}
	return g.NodeRuntime.StartUnit(a, id)
}
func (g *GuardedNodes) StopUnit(a, id string) (unitruntime.State, error) {
	if e := g.Check(); e != nil {
		return unitruntime.State{}, e
	}
	return g.NodeRuntime.StopUnit(a, id)
}
func (g *GuardedNodes) DeleteUnit(a, id string) error {
	if e := g.Check(); e != nil {
		return e
	}
	return g.NodeRuntime.DeleteUnit(a, id)
}
func (g *GuardedNodes) EnsureSource(a, n string, s *source.Manager) error {
	if e := g.Check(); e != nil {
		return e
	}
	return g.NodeRuntime.EnsureSource(a, n, s)
}
func (g *GuardedNodes) RenewLease(a, id, t string, ttl time.Duration) (lease.Record, error) {
	if e := g.Check(); e != nil {
		return lease.Record{}, e
	}
	return g.NodeRuntime.RenewLease(a, id, t, ttl)
}
func (g *GuardedNodes) RevokeLease(a, id, t string) error {
	if e := g.Check(); e != nil {
		return e
	}
	return g.NodeRuntime.RevokeLease(a, id, t)
}

func (g *GuardedNodes) EnsureDisk(a string, c disk.Catalog) error {
	if e := g.Check(); e != nil {
		return e
	}
	n, ok := g.NodeRuntime.(StorageNodes)
	if !ok {
		return fmt.Errorf("storage client unavailable")
	}
	return n.EnsureDisk(a, c)
}
func (g *GuardedNodes) DiskWriter(a, n string) (disk.Writer, error) {
	if e := g.Check(); e != nil {
		return disk.Writer{}, e
	}
	client, ok := g.NodeRuntime.(StorageNodes)
	if !ok {
		return disk.Writer{}, fmt.Errorf("storage client unavailable")
	}
	return client.DiskWriter(a, n)
}

func (g *GuardedNodes) ReleaseDisk(a, n, u, id string) error {
	if e := g.Check(); e != nil {
		return e
	}
	client, ok := g.NodeRuntime.(StorageNodes)
	if !ok {
		return fmt.Errorf("storage client unavailable")
	}
	return client.ReleaseDisk(a, n, u, id)
}

func (g *GuardedNodes) DiskCatalog(a, n string) (disk.Catalog, error) {
	if e := g.Check(); e != nil {
		return disk.Catalog{}, e
	}
	client, ok := g.NodeRuntime.(StorageNodes)
	if !ok {
		return disk.Catalog{}, fmt.Errorf("storage client unavailable")
	}
	return client.DiskCatalog(a, n)
}
