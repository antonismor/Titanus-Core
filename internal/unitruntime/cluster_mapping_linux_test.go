package unitruntime

import "testing"

func TestClusterMappingHandoffAndLocalPoolSeparation(t *testing.T) {
	wanted := IDMapping{Base: 1073741824, Size: 65536}
	for _, root := range []string{t.TempDir(), t.TempDir()} {
		for i := 0; i < 2; i++ {
			m, e := reserveClusterMapping(root, "LAB/db-001-g1", wanted)
			if e != nil || m != wanted {
				t.Fatal("handoff changed mapped ownership", m, e)
			}
		}
		local, e := allocateMapping(root, "local")
		if e != nil || local.Base != 1048576 {
			t.Fatal("cluster mapping exhausted local pool", local, e)
		}
		if _, e = reserveClusterMapping(root, "LAB/other", wanted); e == nil {
			t.Fatal("two identities share mapped ownership")
		}
		if _, e = reserveClusterMapping(root, "LAB/db-001-g1", IDMapping{Base: wanted.Base + 65536, Size: 65536}); e == nil {
			t.Fatal("retained identity mapping changed")
		}
	}
}

func TestInvalidClusterMappingDeniedBeforeExec(t *testing.T) {
	for _, key := range []string{"../unit", "LAB/../unit", "LAB", "LAB/unit/other"} {
		if e := validateClusterMapping(key, IDMapping{Base: 1073741824, Size: 65536}); e == nil {
			t.Fatal("invalid identity admitted", key)
		}
	}
	for _, m := range []IDMapping{{Base: 1048576, Size: 65536}, {Base: 1073741825, Size: 65536}, {Base: 1073741824, Size: 1}, {Base: 2147483647, Size: 65536}} {
		if e := validateClusterMapping("LAB/unit", m); e == nil {
			t.Fatal("unsafe range admitted", m)
		}
	}
}
