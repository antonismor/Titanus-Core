package model

// Native HA has exactly three monitor members, independent of OSD-host count.
// Existing Standard/Custom plans retain their original monitor inventory.
func (p RealmPlan) CephMonitorNodes() []NodeSpec {
	storage := p.StorageNodes()
	if p.Installation != nil && p.Installation.Profile == ProfileNativeHA && len(storage) > 3 {
		return storage[:3]
	}
	return storage
}
func (p RealmPlan) NativeHAStorage() bool {
	return p.Installation != nil && p.Installation.Profile == ProfileNativeHA && p.Ceph.Enabled
}
