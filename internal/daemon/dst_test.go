package daemon

import (
	"testing"
	"time"
)

func TestNextDailyPreservesWallClockAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, day := range []time.Time{
		time.Date(2026, 3, 7, 12, 0, 0, 0, loc),
		time.Date(2026, 10, 31, 12, 0, 0, 0, loc),
	} {
		got := nextDaily(day, "03:30", loc.String())
		want := time.Date(day.Year(), day.Month(), day.Day()+1, 3, 30, 0, 0, loc)
		if !got.Equal(want) {
			t.Errorf("after %s: got %s, want %s", day, got, want)
		}
	}
}
