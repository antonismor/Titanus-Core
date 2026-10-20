package preflight

import (
	"strings"
	"testing"
)

func TestSummaryNamesEveryFailedNodeAndCheckWithoutCommandOutput(t *testing.T) {
	err := Summary([]NodeReport{{Node: "ctl01", Checks: []Check{{Name: "nftables", Details: "PRIVATE CANARY"}, {Name: "OverlayFS"}}}, {Node: "wrk01", Checks: []Check{{Name: "SSH connectivity"}}}, {Node: "healthy", Successful: true}})
	if err == nil {
		t.Fatal("failed checks accepted")
	}
	for _, want := range []string{"ctl01", "nftables", "OverlayFS", "wrk01", "SSH connectivity"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatal(err)
		}
	}
	if strings.Contains(err.Error(), "PRIVATE CANARY") || strings.Contains(err.Error(), "healthy") {
		t.Fatal("unexpected details in summary", err)
	}
}
