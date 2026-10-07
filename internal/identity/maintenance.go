package identity

import (
	"context"
	"log"
	"path/filepath"
	"time"
)

// MaintainCRL keeps the signed policy fresh without discarding revocations.
// The CA key remains on the controller; nodes fetch the public CRL by Pulse.
func (a Authority) MaintainCRL(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		ca, _, err := a.load()
		if err == nil {
			list, loadErr := loadCRL(filepath.Join(a.Dir, "ca.crl"), ca)
			if loadErr != nil || time.Until(list.NextUpdate) < 48*time.Hour {
				_, err = a.Revoke(nil)
			}
		}
		if err != nil {
			log.Printf("Titanus CRL maintenance failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
