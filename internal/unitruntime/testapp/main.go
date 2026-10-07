// A native CI workload with readiness delay and first-run liveness failure.
package main

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	data, _ := os.ReadFile("/data/starts")
	count, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	count++
	if err := os.WriteFile("/data/starts", []byte(fmt.Sprint(count)), 0644); err != nil {
		panic(err)
	}
	started := time.Now()
	http.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(started) < 2*time.Second {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	http.HandleFunc("/live", func(w http.ResponseWriter, r *http.Request) {
		if count == 1 && time.Since(started) > 4*time.Second {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(200)
	})
	fmt.Printf("HEALTH_START_%d\n", count)
	if err := http.ListenAndServe("127.0.0.1:8080", nil); err != nil {
		panic(err)
	}
}
