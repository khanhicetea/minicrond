//go:build linux

package procstats

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const Supported = true

// Linux procfs exposes CPU ticks in USER_HZ (100 on supported Linux amd64/
// arm64), independent of the kernel's scheduler HZ configuration.
const tickUS = 10_000

func Read(pid int, expectedStartID string) (*Sample, error) {
	if pid <= 0 {
		return nil, errors.New("process not started")
	}
	f, err := os.Open(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, fmt.Errorf("open process stats: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return nil, fmt.Errorf("read process stats: %w", err)
	}
	if len(b) > 4096 {
		return nil, errors.New("process stats exceed limit")
	}
	sample, err := parseStat(string(b), int64(os.Getpagesize()))
	if err != nil {
		return nil, err
	}
	if expectedStartID != "" && sample.StartID != expectedStartID {
		return nil, errors.New("process identity changed")
	}
	sample.SampledAt = time.Now().UTC()
	return sample, nil
}

func parseStat(stat string, pageBytes int64) (*Sample, error) {
	_, rest, ok := strings.CutLast(stat, ")")
	fields := strings.Fields(rest)
	if !ok || len(fields) < 22 {
		return nil, errors.New("invalid process stats")
	}
	if fields[0] == "Z" || fields[0] == "X" {
		return nil, errors.New("process exited")
	}
	user, err := strconv.ParseInt(fields[11], 10, 64) // field 14
	if err != nil {
		return nil, fmt.Errorf("parse user CPU: %w", err)
	}
	system, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse system CPU: %w", err)
	}
	rss, err := strconv.ParseInt(fields[21], 10, 64) // field 24
	if err != nil {
		return nil, fmt.Errorf("parse RSS: %w", err)
	}
	// Reject malformed/overflowing counters rather than wrapping them.
	const maxInt64 = int64(1<<63 - 1)
	if user < 0 || system < 0 || user > maxInt64/tickUS-system || rss < 0 || pageBytes <= 0 || rss > maxInt64/pageBytes {
		return nil, errors.New("invalid process counters")
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return nil, fmt.Errorf("parse process identity: %w", err)
	}
	return &Sample{StartID: fields[19], CPUUS: (user + system) * tickUS, RSSBytes: rss * pageBytes}, nil
}
