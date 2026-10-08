package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/localclient"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

func runOrchestration(kind string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus %s <list|submit|apply|status|cancel|delete|put|keygen> ...", kind)
	}
	if kind == "secret" && args[0] == "keygen" {
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus secret keygen <new-private-file>")
		}
		f, e := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		key := make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			f.Close()
			os.Remove(args[1])
			return e
		}
		k := secrets.Keyring{Active: "key1", Keys: map[string][]byte{"key1": key}}
		data, e := json.Marshal(k)
		if e == nil {
			_, e = f.Write(append(data, '\n'))
		}
		if e == nil {
			e = f.Sync()
		}
		closeErr := f.Close()
		if e != nil {
			os.Remove(args[1])
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		dir, e := os.Open(filepath.Dir(args[1]))
		if e != nil {
			return e
		}
		e = dir.Sync()
		dir.Close()
		if e != nil {
			return e
		}
		fmt.Println("Private encryption keyring created; provision it separately on authorized controllers and execution nodes.")
		return nil
	}
	endpoint := map[string]string{"task": "tasks", "autoscale": "autoscalers", "secret": "secrets"}[kind]
	path := "/v1/realm/" + endpoint
	method := http.MethodGet
	var body any
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("list takes no arguments")
		}
	case "submit", "apply":
		if len(args) != 2 || kind == "secret" {
			return fmt.Errorf("usage: titanus %s %s <json-file>", kind, args[0])
		}
		raw, e := os.ReadFile(args[1])
		if e != nil {
			return e
		}
		if !json.Valid(raw) {
			return fmt.Errorf("invalid JSON")
		}
		body = json.RawMessage(raw)
		method = http.MethodPost
	case "status", "cancel", "delete":
		if len(args) != 2 {
			return fmt.Errorf("object name required")
		}
		path += "/" + url.PathEscape(args[1])
		if args[0] == "cancel" {
			if kind != "task" {
				return fmt.Errorf("cancel applies to Tasks")
			}
			path += "/cancel"
			method = http.MethodPost
		} else if args[0] == "delete" {
			if kind == "task" {
				return fmt.Errorf("Task execution records are retained")
			}
			method = http.MethodDelete
		}
	case "put":
		if kind != "secret" || len(args) != 2 || !secrets.Name.MatchString(args[1]) {
			return fmt.Errorf("usage: titanus secret put <name> < private-input-file")
		}
		data, e := io.ReadAll(io.LimitReader(os.Stdin, 16385))
		if e != nil {
			return e
		}
		if len(data) == 0 || len(data) > 16384 {
			return fmt.Errorf("secret must contain 1–16384 bytes")
		}
		defer clear(data)
		body = map[string]string{"value": base64.StdEncoding.EncodeToString(data)}
		method = http.MethodPut
		path += "/" + url.PathEscape(args[1])
	default:
		return fmt.Errorf("unknown orchestration action")
	}
	result, e := localclient.New(os.Getenv("TITANUS_SOCKET")).Orchestration(method, path, body)
	if e != nil {
		return e
	}
	fmt.Println(string(result))
	return nil
}
