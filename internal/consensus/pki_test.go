package consensus

import (
	"github.com/antonismor/Titanus-Core/internal/identity"
	"math/big"
	"testing"
)

func TestPKISigningSequenceAndRevocationsSurviveLeaderLoss(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	policy, err := c.stores[leader].MutatePKI(func(identity.Policy) (identity.Policy, error) { return c.authority.InitialPolicy() })
	if err != nil {
		t.Fatal(err)
	}
	c.converge(c.stores[leader].Snapshot().Revision)
	if _, err = c.stores[(leader+1)%3].MutatePKI(func(p identity.Policy) (identity.Policy, error) { t.Fatal("follower signed"); return p, nil }); err == nil {
		t.Fatal("follower acknowledged issuance")
	}
	policy, err = c.stores[leader].MutatePKI(func(p identity.Policy) (identity.Policy, error) {
		p.Issued++
		return c.authority.SignPolicy(p, big.NewInt(123))
	})
	if err != nil {
		t.Fatal(err)
	}
	serial, err := policy.NextSerial()
	if err != nil {
		t.Fatal(err)
	}
	c.converge(c.stores[leader].Snapshot().Revision)
	c.nodes[leader].Close()
	c.nodes[leader] = nil
	next := c.leader(leader)
	if !c.stores[next].Snapshot().PKI.Revoked(big.NewInt(123)) {
		t.Fatal("revocation lost on signer failover")
	}
	policy, err = c.stores[next].MutatePKI(func(p identity.Policy) (identity.Policy, error) {
		p.Issued++
		return c.authority.SignPolicy(p, big.NewInt(456))
	})
	if err != nil {
		t.Fatal(err)
	}
	newSerial, err := policy.NextSerial()
	if err != nil || newSerial.Cmp(serial) <= 0 {
		t.Fatal("issuance sequence rolled back", err)
	}
	if !policy.Revoked(big.NewInt(123)) || !policy.Revoked(big.NewInt(456)) {
		t.Fatal("new signer discarded prior revocations")
	}
}
