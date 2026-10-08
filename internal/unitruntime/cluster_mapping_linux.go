package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func validateClusterMapping(key string, m IDMapping) error {
	if key == "" && m == (IDMapping{}) {
		return nil
	}
	parts := strings.Split(key, "/")
	if len(parts) != 2 || !objectName.MatchString(parts[0]) || !objectName.MatchString(parts[1]) || m.Size != mappingSize || m.Base < 1073741824 || m.Base > 2147418112 || m.Base%mappingSize != 0 {
		return fmt.Errorf("invalid cluster mapping identity")
	}
	return nil
}

func reserveClusterMapping(root, key string, wanted IDMapping) (IDMapping, error) {
	if err := validateClusterMapping(key, wanted); err != nil {
		return IDMapping{}, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return IDMapping{}, err
	}
	f, err := os.OpenFile(filepath.Join(root, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return IDMapping{}, err
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return IDMapping{}, err
	}
	path := filepath.Join(root, "allocations.json")
	ledger := map[string]IDMapping{}
	if err = loadJSON(path, &ledger); err != nil && !os.IsNotExist(err) {
		return IDMapping{}, err
	}
	key = "cluster:" + key
	seen := map[int]bool{}
	for k, m := range ledger {
		if m.Size != mappingSize || m.Base < 1048576 || m.Base > 2147418112 || m.Base%mappingSize != 0 || seen[m.Base] {
			return IDMapping{}, fmt.Errorf("invalid mapping ledger")
		}
		seen[m.Base] = true
		if k == key {
			if m != wanted {
				return IDMapping{}, fmt.Errorf("cluster identity changed its mapping")
			}
		} else if m.Base == wanted.Base {
			return IDMapping{}, fmt.Errorf("cluster mapping conflicts with retained node identity; offline migration required")
		}
	}
	if ledger[key] == wanted {
		return wanted, nil
	}
	ledger[key] = wanted
	if err = saveJSON(path, ledger, 0600); err != nil {
		return IDMapping{}, err
	}
	return wanted, nil
}
