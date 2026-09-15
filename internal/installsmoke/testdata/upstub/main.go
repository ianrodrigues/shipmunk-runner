// Command upstub is a minimal loopback HTTP responder used only to satisfy
// the runner setup's /up reachability check inside a container fixture that
// deliberately has no other HTTP server available. It is test-harness
// infrastructure standing in for a real Shipmunk server; it is not part of
// the runner release and is never published. This directory is testdata so
// the module's own build, vet, and test targets never compile it as a
// module command; internal/installsmoke and CI build it explicitly for the
// target container platform.
//
// Every request it receives, on any path, is appended to requestLogPath so a
// test can confirm the installed binaries genuinely reached this server over
// the network, rather than failing earlier at a local preflight step.
package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const requestLogPath = "/tmp/upstub-requests.log"

func main() {
	const addr = "127.0.0.1:8080"
	if len(os.Args) > 1 && os.Args[1] == "-wait" {
		waitReady(addr)
		return
	}
	var mu sync.Mutex
	logRequest := func(r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		file, err := os.OpenFile(requestLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return
		}
		defer file.Close()
		fmt.Fprintf(file, "%s %s\n", r.Method, r.URL.Path)
	}
	http.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) {
		logRequest(r)
		w.WriteHeader(http.StatusOK)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logRequest(r)
		http.NotFound(w, r)
	})
	_ = http.ListenAndServe(addr, nil)
}

func waitReady(addr string) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if connection, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = connection.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(1)
}
