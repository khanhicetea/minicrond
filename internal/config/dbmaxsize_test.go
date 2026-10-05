package config

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

// A17: MiB values are bounded before they are shifted into bytes.
func TestDBMaxSizeBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "minicron.toml")
	try := func(value int64) error {
		mustWrite(t, path, fmt.Sprintf("[logs]\ndb_max_size=%d\n", value))
		_, err := Load(path)
		return err
	}
	for _, ok := range []int64{0, 1, 2048, MaxDBMaxSizeMiB} {
		if err := try(ok); err != nil {
			t.Errorf("db_max_size=%d rejected: %v", ok, err)
		}
	}
	bad := []int64{
		-1,
		MaxDBMaxSizeMiB + 1,
		math.MaxInt64>>20 + 1, // 1<<43: shifts to math.MinInt64
		1 << 44,               // wraps to exactly zero (budget silently disabled)
		1<<44 + 1,             // wraps to a 1 MiB budget
		1<<44 + 5,             // wraps to a tiny positive budget
		1<<43 + 1<<20,         // wraps to a negative budget
		math.MaxInt64,
	}
	for _, value := range bad {
		err := try(value)
		if err == nil || !strings.Contains(err.Error(), "logs.db_max_size") {
			t.Errorf("db_max_size=%d accepted or wrong error: %v", value, err)
		}
	}
}

func TestMaxSizeBytesSaturatesInsteadOfWrapping(t *testing.T) {
	cases := []struct {
		mib  int
		want int64
	}{
		{0, 0},
		{-1, 0},
		{1, 1 << 20},
		{2048, 2048 << 20},
		{MaxDBMaxSizeMiB, int64(MaxDBMaxSizeMiB) << 20},
	}
	// These wrapped to MinInt64, 0 and tiny budgets before the guard.
	for _, mib := range []int64{math.MaxInt64>>20 + 1, 1 << 44, 1<<44 + 5, math.MaxInt64} {
		if int64(int(mib)) == mib { // skip values a 32-bit int cannot hold
			cases = append(cases, struct {
				mib  int
				want int64
			}{int(mib), math.MaxInt64})
		}
	}
	for _, tc := range cases {
		if got := (Logs{DBMaxSize: tc.mib}).MaxSizeBytes(); got != tc.want {
			t.Errorf("MaxSizeBytes(%d) = %d, want %d", tc.mib, got, tc.want)
		}
	}
}
