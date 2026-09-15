// Command upstub is a minimal loopback HTTP responder standing in for a real
// Shipmunk server in the install smoke test. It is testdata, so the module's
// own build, vet, and test targets never compile it as a module command.
//
// Every request, on any path, is appended to requestLogPath.
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

// addrEnv overrides the default loopback address, so an unrelated process
// already on 127.0.0.1:8080 cannot be mistaken for the up-stub.
const addrEnv = "SHIPMUNK_UPSTUB_ADDR"

func main() {
	addr := os.Getenv(addrEnv)
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
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
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Fprintln(os.Stderr, "upstub: cannot bind "+addr+": "+err.Error())
		os.Exit(1)
	}
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
