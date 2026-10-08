// Package secrets encrypts immutable, version-pinned workload values. Keys are
// provisioned separately from Realm state; no API exports plaintext or keys.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"
)

var Name = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,47}$`)
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)

type Ref struct {
	Name        string `json:"name"`
	Version     uint64 `json:"version"`
	Environment string `json:"environment"`
}
type Record struct {
	Realm      string `json:"realm"`
	Name       string `json:"name"`
	Version    uint64 `json:"version"`
	KeyID      string `json:"key_id"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}
type Binding struct {
	Ref    Ref    `json:"ref"`
	Record Record `json:"record"`
}
type Keyring struct {
	Active string            `json:"active"`
	Keys   map[string][]byte `json:"keys"`
}

func Load(path string) (*Keyring, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, fmt.Errorf("secret keyring unavailable")
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 16384 {
		return nil, fmt.Errorf("keyring must be a private regular file")
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("keyring must be owned by daemon identity")
	}
	var k Keyring
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if e = dec.Decode(&k); e != nil {
		return nil, fmt.Errorf("invalid secret keyring")
	}
	var trailing any
	if e = dec.Decode(&trailing); e != io.EOF {
		return nil, fmt.Errorf("invalid trailing keyring data")
	}
	if len(k.Keys) < 1 || len(k.Keys) > 16 || len(k.Keys[k.Active]) != 32 {
		return nil, fmt.Errorf("invalid active encryption key")
	}
	for id, v := range k.Keys {
		if !Name.MatchString(id) || len(v) != 32 {
			return nil, fmt.Errorf("invalid encryption key")
		}
	}
	return &k, nil
}
func aad(r Record) []byte {
	b, _ := json.Marshal([]any{"titanus-secret/v1", r.Realm, r.Name, r.Version, r.KeyID})
	return b
}
func (k *Keyring) aead(id string) (cipher.AEAD, error) {
	if k == nil || len(k.Keys[id]) != 32 {
		return nil, fmt.Errorf("secret key unavailable")
	}
	b, e := aes.NewCipher(k.Keys[id])
	if e != nil {
		return nil, e
	}
	return cipher.NewGCM(b)
}
func (k *Keyring) Encrypt(realm, name string, version uint64, value []byte) (Record, error) {
	if realm == "" || !Name.MatchString(name) || version == 0 || len(value) == 0 || len(value) > 16384 || strings.ContainsRune(string(value), 0) {
		return Record{}, fmt.Errorf("invalid secret value or identity")
	}
	if k == nil {
		return Record{}, fmt.Errorf("secret key unavailable")
	}
	r := Record{Realm: realm, Name: name, Version: version, KeyID: k.Active}
	a, e := k.aead(r.KeyID)
	if e != nil {
		return Record{}, e
	}
	r.Nonce = make([]byte, a.NonceSize())
	if _, e = rand.Read(r.Nonce); e != nil {
		return Record{}, e
	}
	r.Ciphertext = a.Seal(nil, r.Nonce, value, aad(r))
	return r, nil
}
func (k *Keyring) Decrypt(r Record) ([]byte, error) {
	a, e := k.aead(r.KeyID)
	if e != nil {
		return nil, e
	}
	if len(r.Nonce) != a.NonceSize() || len(r.Ciphertext) > 16400 {
		return nil, fmt.Errorf("invalid encrypted secret")
	}
	v, e := a.Open(nil, r.Nonce, r.Ciphertext, aad(r))
	if e != nil {
		return nil, fmt.Errorf("secret authentication failed")
	}
	return v, nil
}
func ValidateRefs(refs []Ref, env []string) error {
	if len(refs) > 32 {
		return fmt.Errorf("too many secret references")
	}
	seen := map[string]bool{}
	for _, v := range env {
		seen[strings.SplitN(v, "=", 2)[0]] = true
	}
	for _, r := range refs {
		if !Name.MatchString(r.Name) || r.Version == 0 || !envName.MatchString(r.Environment) || seen[r.Environment] {
			return fmt.Errorf("invalid or duplicate secret reference")
		}
		seen[r.Environment] = true
	}
	return nil
}
func (k *Keyring) Environment(realm string, refs []Ref, bindings []Binding, env []string) ([]string, error) {
	if e := ValidateRefs(refs, env); e != nil {
		return nil, e
	}
	if len(refs) != len(bindings) {
		return nil, fmt.Errorf("incomplete encrypted secret bindings")
	}
	out := append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, env...)
	for i, r := range refs {
		b := bindings[i]
		if b.Ref != r || b.Record.Realm != realm || b.Record.Name != r.Name || b.Record.Version != r.Version {
			return nil, fmt.Errorf("secret binding identity mismatch")
		}
		v, e := k.Decrypt(b.Record)
		if e != nil {
			return nil, e
		}
		out = append(out, r.Environment+"="+string(v))
		clear(v)
	}
	return out, nil
}
