package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/antonismor/Titanus-Core/internal/source"
)

const mappingSize = 65536
const mappingRoot = "/var/lib/titanus-userns"

type IDMapping struct {
	Base int `json:"base"`
	Size int `json:"size"`
}

// Node-global, fsynced, monotonic ledger. Deleted allocations are never recycled:
// a later Unit cannot acquire ownership of a previous Unit's persistent files.
func (m *Manager) allocateMapping(id string) (IDMapping, error) {
	_, state, e := m.load(id)
	if e != nil {
		return IDMapping{}, e
	}
	root, e := filepath.EvalSymlinks(m.cfg.StateRoot)
	if e != nil {
		return IDMapping{}, e
	}
	root, e = filepath.Abs(root)
	if e != nil {
		return IDMapping{}, e
	}
	return allocateMapping(mappingRoot, filepath.Join(root, "units", id)+"@"+state.CreatedAt.Format("20060102T150405.000000000Z"), state.UserMapping)
}
func allocateMapping(root, key string, expected ...IDMapping) (IDMapping, error) {
	if e := os.MkdirAll(root, 0700); e != nil {
		return IDMapping{}, e
	}
	lock, e := os.OpenFile(filepath.Join(root, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return IDMapping{}, e
	}
	defer lock.Close()
	if e := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return IDMapping{}, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ledger := map[string]IDMapping{}
	path := filepath.Join(root, "allocations.json")
	if e := loadJSON(path, &ledger); e != nil && !os.IsNotExist(e) {
		return IDMapping{}, e
	}
	base := 1048576
	seen := map[int]bool{}
	for _, v := range ledger {
		if v.Size != mappingSize || v.Base < 1048576 || v.Base%mappingSize != 0 || v.Base > 2147418112 {
			return IDMapping{}, fmt.Errorf("invalid mapping ledger")
		}
		if seen[v.Base] {
			return IDMapping{}, fmt.Errorf("overlapping mapping ledger")
		}
		seen[v.Base] = true
		if v.Base >= base {
			base = v.Base + mappingSize
		}
	}
	if v, ok := ledger[key]; ok {
		if len(expected) > 0 && expected[0].Size != 0 && expected[0] != v {
			return IDMapping{}, fmt.Errorf("persisted Unit mapping disagrees with node ledger")
		}
		return v, nil
	}
	if len(expected) > 0 && expected[0].Size != 0 {
		return IDMapping{}, fmt.Errorf("persisted Unit mapping is missing from node ledger; restore ledger before restart")
	}
	if base > 2147418112 {
		return IDMapping{}, fmt.Errorf("node UID/GID mapping pool exhausted")
	}
	v := IDMapping{base, mappingSize}
	ledger[key] = v
	if e := saveJSON(path, ledger, 0600); e != nil {
		return IDMapping{}, e
	}
	return v, nil
}

func (m *Manager) prepareMappedSource(spec Spec, mapping IDMapping) (string, error) {
	target := filepath.Join(m.unitDir(spec.ID), "mapped-source")
	if info, e := os.Stat(target); e == nil && info.IsDir() {
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != uint32(mapping.Base) || st.Gid != uint32(mapping.Base) {
			return "", fmt.Errorf("mapped Source ownership disagrees with Unit mapping")
		}
		return target, nil
	}
	temporary := target + ".tmp"
	_ = os.RemoveAll(temporary)
	if e := source.CopyMapped(m.sourceRoot(spec.Source), temporary, mapping.Base, mapping.Size); e != nil {
		_ = os.RemoveAll(temporary)
		return "", e
	}
	for _, d := range []string{"upper", "work"} {
		entries, e := os.ReadDir(filepath.Join(m.unitDir(spec.ID), d))
		if e != nil {
			return "", e
		}
		if len(entries) > 0 {
			return "", fmt.Errorf("legacy upper/work layer requires offline ownership migration or Unit recreation")
		}
		if e := os.Chown(filepath.Join(m.unitDir(spec.ID), d), mapping.Base, mapping.Base); e != nil {
			return "", e
		}
	}
	if e := os.Rename(temporary, target); e != nil {
		return "", e
	}
	return target, nil
}
