package api

import (
	"context"
	"io"
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

func TestPeerListenerRejectsWrongUID(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is intentionally allowed to access every local socket")
	}
	path := filepath.Join(t.TempDir(), "minicron.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	finished := make(chan error, 1)
	go func() {
		_, err := (peerListener{Listener: ln, uid: uint32(os.Geteuid() + 1)}).Accept()
		finished <- err
	}()
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if _, err := conn.Read(one[:]); err != io.EOF {
		t.Fatalf("wrong-UID peer should be disconnected, got %v", err)
	}
	_ = ln.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("peer listener did not stop")
	}
}
