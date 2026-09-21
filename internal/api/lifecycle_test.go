package api

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStartFailureReleasesTCPListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil, nil, nil, "test")
	socket := filepath.Join(t.TempDir(), "missing", "minicron.sock")
	if err := s.Start(addr, socket); err == nil {
		_ = s.Shutdown(context.Background())
		t.Fatal("expected Unix listener failure")
	}
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("TCP listener leaked: %v", err)
	}
	_ = ln.Close()
}

func TestPeerUID(t *testing.T) {
	// Keep the path short enough for Darwin's sockaddr_un.
	dir, err := os.MkdirTemp("/tmp", "minicron-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "socket")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := ln.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	uid, err := peerUID(conn)
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Geteuid()) {
		t.Fatalf("got UID %d, want %d", uid, os.Geteuid())
	}
}
