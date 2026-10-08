package secrets

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthenticationRotationAndPinnedEnvironment(t *testing.T) {
	k := &Keyring{Active: "key1", Keys: map[string][]byte{"key1": bytes.Repeat([]byte{1}, 32), "key2": bytes.Repeat([]byte{2}, 32)}}
	r, e := k.Encrypt("LAB", "database", 1, []byte("CANARY_PASSWORD"))
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(r)
	if bytes.Contains(raw, []byte("CANARY_PASSWORD")) {
		t.Fatal("plaintext persisted")
	}
	k.Active = "key2"
	v, e := k.Decrypt(r)
	if e != nil || string(v) != "CANARY_PASSWORD" {
		t.Fatal("old key version unavailable")
	}
	for _, mutate := range []func(*Record){func(r *Record) { r.Name = "other" }, func(r *Record) { r.Realm = "OTHER" }, func(r *Record) { r.Version++ }, func(r *Record) { r.KeyID = "key2" }, func(r *Record) { r.Ciphertext = append([]byte(nil), r.Ciphertext...); r.Ciphertext[0] ^= 1 }} {
		altered := r
		mutate(&altered)
		if _, e = k.Decrypt(altered); e == nil {
			t.Fatal("tampered record accepted")
		}
	}
	ref := Ref{Name: "database", Version: 1, Environment: "PASSWORD"}
	env, e := k.Environment("LAB", []Ref{ref}, []Binding{{Ref: ref, Record: r}}, nil)
	if e != nil || !strings.Contains(strings.Join(env, "\n"), "PASSWORD=CANARY_PASSWORD") {
		t.Fatal(e)
	}
	if _, e = k.Environment("OTHER", []Ref{ref}, []Binding{{Ref: ref, Record: r}}, nil); e == nil {
		t.Fatal("wrong Realm accepted")
	}
	if e = ValidateRefs([]Ref{ref}, []string{"PASSWORD=other"}); e == nil {
		t.Fatal("duplicate environment accepted")
	}
	delete(k.Keys, "key1")
	if _, e = k.Decrypt(r); e == nil {
		t.Fatal("missing key accepted")
	}
}
func TestKeyringFilePolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys")
	k := Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{3}, 32)}}
	raw, _ := json.Marshal(k)
	os.WriteFile(path, raw, 0644)
	if _, e := Load(path); e == nil {
		t.Fatal("world-readable keyring accepted")
	}
	os.Chmod(path, 0600)
	if _, e := Load(path); e != nil {
		t.Fatal(e)
	}
}
