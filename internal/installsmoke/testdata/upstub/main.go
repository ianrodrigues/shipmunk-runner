// Command upstub is a minimal loopback HTTP responder used only to satisfy
// the runner setup's /up reachability check inside a container fixture that
// deliberately has no other HTTP server available. It is test-harness
// infrastructure standing in for a real Shipmunk server; it is not part of
// the runner release and is never published. This directory is testdata so
// the module's own build, vet, and test targets never compile it as a
// module command; internal/installsmoke and CI build it explicitly for the
// target container platform.
package main

import (
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	const addr = "127.0.0.1:8080"
	if len(os.Args) > 1 && os.Args[1] == "-wait" {
		waitReady(addr)
		return
	}
	http.HandleFunc("/up", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
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
