package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestCLIReportsOutputFailures(t *testing.T) {
	for _, command := range []string{"schema", "version", "help"} {
		t.Run(command, func(t *testing.T) {
			closed, err := os.CreateTemp(t.TempDir(), "closed-output")
			if err != nil {
				t.Fatal(err)
			}
			if err := closed.Close(); err != nil {
				t.Fatal(err)
			}
			originalOutput, originalArgs := os.Stdout, os.Args
			os.Stdout, os.Args = closed, []string{"minicrond", command}
			t.Cleanup(func() { os.Stdout, os.Args = originalOutput, originalArgs })
			if err := run(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("closed output reported success: %v", err)
			}
		})
	}
}

func TestLogsRejectInvalidPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.WriteString(w, `{"items":[{"sequence":1,"stream":1,"payload":"invalid base64"}]}`)
		if err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("MINICRON_URL", server.URL)
	if err := logs([]string{"run"}); err == nil || !strings.Contains(err.Error(), "decode log frame 1") {
		t.Fatalf("invalid log payload was accepted: %v", err)
	}
}

func TestRequestPreservesHTTPStatusAndBodyReadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusInternalServerError)
		if _, err := io.WriteString(w, "error"); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("MINICRON_URL", server.URL)
	var out any
	err := requestJSON("GET", "/test", nil, &out)
	if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("HTTP status or body error was lost: %v", err)
	}
}

type closeFailureBody struct{ error }

func (b closeFailureBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b closeFailureBody) Close() error             { return b.error }

func TestResponseCleanupPreservesBothFailures(t *testing.T) {
	readErr, closeErr := errors.New("read failure"), errors.New("close failure")
	result := readErr
	closeResponse(closeFailureBody{closeErr}, &result)
	if !errors.Is(result, readErr) || !errors.Is(result, closeErr) {
		t.Fatalf("response errors lost during cleanup: %v", result)
	}
}
