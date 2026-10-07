package reconcile

import (
	"errors"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"testing"
	"time"
)

func TestQuorumLossBlocksAllPhysicalNodeActions(t *testing.T) {
	denied := errors.New("quorum lost")
	// A nil underlying runtime proves that none of these calls gets forwarded.
	g := &GuardedNodes{Check: func() error { return denied }}
	if _, e := g.EnsureUnit("node", unitruntime.Spec{}); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if _, e := g.StartUnit("node", "unit"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if _, e := g.StopUnit("node", "unit"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if e := g.DeleteUnit("node", "unit"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if e := g.EnsureSource("node", "source", nil); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if _, e := g.RenewLease("node", "unit", "token", 20*time.Second); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if e := g.RevokeLease("node", "unit", "token"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
}
