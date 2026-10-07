package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

func TestChildRejectsUnsafePolicyBeforeNamespaceSetup(t *testing.T) {
	for _, policy := range []string{`{"no_new_privs":false}`, `{"capabilities":["SYS_ADMIN"]}`, `{broken`} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		args := []string{"/nonexistent", "unit", "3", policy, fmt.Sprint(w.Fd()), "--", "/bin/sh"}
		if err := runUnitChild(args); err == nil {
			t.Fatal("unsafe policy accepted")
		}
		data, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), "ERROR:") || strings.Contains(string(data), "READY") {
			t.Fatalf("incorrect failure handshake: %q", data)
		}
	}
}
