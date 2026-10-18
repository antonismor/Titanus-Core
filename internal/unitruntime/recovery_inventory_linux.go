package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
)

// EmptyRecoveryInventory is deliberately stricter than diagnostic List, which
// skips unreadable Unit records. Recovery must reject every retained entry,
// including corrupt/redirected records whose runtime status cannot be read.
func (m *Manager) EmptyRecoveryInventory() error {
	root := filepath.Join(m.cfg.StateRoot, "units")
	st, err := os.Lstat(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("redirected/invalid Unit recovery inventory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("retained Unit inventory prevents recovery")
	}
	return nil
}
