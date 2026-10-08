package version

import (
	"encoding/json"
	"fmt"
	"runtime"
)

var Version = "0.4.0-rc.2"
var Revision = "unknown"

const StateProfile = "titanus-state/v2"

type Build struct {
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	StateProfile string `json:"state_profile"`
}

func Info() Build { return Build{Version, Revision, runtime.GOOS, runtime.GOARCH, StateProfile} }
func Print(jsonOutput bool) {
	if jsonOutput {
		b, _ := json.Marshal(Info())
		fmt.Println(string(b))
	} else {
		fmt.Printf("Titanus Core %s (%s; %s/%s)\n", Version, Revision, runtime.GOOS, runtime.GOARCH)
	}
}
