package main

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

func TestMultipathTCPDisabledByDefault(t *testing.T) {
	// Clear any runtime override to check the default embedded by go.mod.
	t.Setenv("GODEBUG", "")
	var dialer net.Dialer
	if dialer.MultipathTCP() {
		t.Fatal("default dialer must not use MPTCP")
	}
	var listener net.ListenConfig
	if listener.MultipathTCP() {
		t.Fatal("default listener must not use MPTCP")
	}
}

func TestLocalCLIUsesDataDirectorySocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MINICROND_DATA", dir)
	t.Setenv("MINICROND_URL", "")
	t.Setenv("HOME", "/root")
	listener, err := net.Listen("unix", filepath.Join(dir, "minicron.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/daemon" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"test"}`))
	})}
	go server.Serve(listener)
	defer server.Shutdown(context.Background())
	var status struct {
		Version string `json:"version"`
	}
	if err := requestJSON("GET", "/api/v1/daemon", nil, &status); err != nil || status.Version != "test" {
		t.Fatalf("local CLI socket: version=%q err=%v", status.Version, err)
	}
}
