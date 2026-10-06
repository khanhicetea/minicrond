//go:build linux

package procstats

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func statFixture() []string {
	fields := make([]string, 22)
	for i := range fields {
		fields[i] = "0"
	}
	fields[0], fields[11], fields[12], fields[19], fields[21] = "S", "123", "45", "999", "100"
	return fields
}

func TestParseStat(t *testing.T) {
	for _, tc := range []struct {
		name    string
		modify  func([]string)
		wantErr bool
	}{
		{"valid with tricky command", func([]string) {}, false},
		{"zombie", func(f []string) { f[0] = "Z" }, true},
		{"invalid CPU", func(f []string) { f[11] = "no" }, true},
		{"negative RSS", func(f []string) { f[21] = "-1" }, true},
		{"overflow CPU", func(f []string) { f[12] = "9223372036854775807" }, true},
		{"overflow RSS", func(f []string) { f[21] = "9223372036854775807" }, true},
		{"invalid identity", func(f []string) { f[19] = "no" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := statFixture()
			tc.modify(fields)
			got, err := parseStat("123 (name ) with ( spaces)) "+strings.Join(fields, " "), 4096)
			if (err != nil) != tc.wantErr {
				t.Fatalf("stats=%+v err=%v", got, err)
			}
			if !tc.wantErr && (got.CPUUS != 1680000 || got.RSSBytes != 409600 || got.StartID != "999") {
				t.Fatalf("stats=%+v", got)
			}
		})
	}
	for _, s := range []string{"", "123 (x) S 1", "123 x S"} {
		if _, err := parseStat(s, 4096); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestReadSelfChecksIdentity(t *testing.T) {
	got, err := Read(os.Getpid(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got.SampledAt.IsZero() || got.RSSBytes <= 0 {
		t.Fatalf("sample=%+v", got)
	}
	if _, err := Read(os.Getpid(), got.StartID); err != nil {
		t.Fatal(err)
	}
	start, err := strconv.ParseUint(got.StartID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(os.Getpid(), strconv.FormatUint(start+1, 10)); err == nil {
		t.Fatal("accepted reused PID")
	}
	if _, err := Read(0, ""); err == nil {
		t.Fatal("accepted missing PID")
	}
}
