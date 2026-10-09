package secrets

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"os"
	"syscall"
)

// Provision never replaces or removes a key. Retained specs/backups may still
// hold the old ciphertext even after replicated vault re-encryption succeeds.
func Provision(path, id string, value []byte, activate bool) error {
	if !Name.MatchString(id) {
		return fmt.Errorf("invalid key identity")
	}
	fd, e := syscall.Open(path+".lock", syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return e
	}
	lock := os.NewFile(uintptr(fd), path+".lock")
	defer lock.Close()
	st, e := lock.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe encryption provisioning lock")
	}
	if e = syscall.Flock(fd, syscall.LOCK_EX); e != nil {
		return e
	}
	keys, e := Load(path)
	if e != nil {
		return e
	}
	if activate {
		if len(keys.Keys[id]) != 32 {
			return fmt.Errorf("key must be pre-provisioned before activation")
		}
		keys.Active = id
	} else {
		if len(value) != 32 || len(keys.Keys) >= 16 || keys.Keys[id] != nil {
			return fmt.Errorf("new distinct 32-byte key required; retained keys cannot be replaced")
		}
		keys.Keys[id] = append([]byte(nil), value...)
	}
	return durable.WriteJSON(path, keys, 0600)
}
