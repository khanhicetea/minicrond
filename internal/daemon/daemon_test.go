package daemon

import (
	"testing"
	"time"
)

func TestNextDaily(t *testing.T) {
	utc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2025, 6, 1, 10, 0, 0, 0, utc)
	if got := nextDaily(now, "03:30", "UTC"); !got.Equal(time.Date(2025, 6, 2, 3, 30, 0, 0, utc)) {
		t.Fatalf("later today already passed: %v", got)
	}
	if got := nextDaily(now, "23:45", "UTC"); !got.Equal(time.Date(2025, 6, 1, 23, 45, 0, 0, utc)) {
		t.Fatalf("later today still ahead: %v", got)
	}
	exact := time.Date(2025, 6, 1, 3, 30, 0, 0, utc)
	if got := nextDaily(exact, "03:30", "UTC"); !got.Equal(exact.Add(24 * time.Hour)) {
		t.Fatalf("exact match must roll to tomorrow: %v", got)
	}
	// Garbage falls back to the 03:30 default rather than spinning.
	if got := nextDaily(now, "garbage", "UTC"); !got.Equal(time.Date(2025, 6, 2, 3, 30, 0, 0, utc)) {
		t.Fatalf("invalid clock: %v", got)
	}
	// A non-UTC scheduler timezone shifts the wall clock.
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	got := nextDaily(now, "09:00", "Asia/Tokyo")
	if got.In(tokyo).Hour() != 9 || got.In(tokyo).Day() != 2 {
		t.Fatalf("tokyo scheduling: %v", got)
	}
}
