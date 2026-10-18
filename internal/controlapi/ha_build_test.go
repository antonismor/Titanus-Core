package controlapi

import (
	"github.com/antonismor/Titanus-Core/internal/version"
	"testing"
)

func TestHAReadinessAcceptsBothNativeArchitecturesAtSameBuild(t *testing.T) {
	b := version.Info()
	for _, arch := range []string{"amd64", "arm64"} {
		b.Arch = arch
		if !compatibleHABuild(b) {
			t.Fatal("native architecture rejected", arch)
		}
	}
	b.Revision = "other"
	if compatibleHABuild(b) {
		t.Fatal("different build accepted")
	}
}
