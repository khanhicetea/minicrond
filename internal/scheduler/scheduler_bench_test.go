package scheduler

import (
	"testing"
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func BenchmarkNextFire(b *testing.B) {
	d := model.Definition{Schedule: "*/5 9-17 * * 1-5", Timezone: "UTC"}
	after := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for b.Loop() {
		if _, err := nextFire(d, after, time.Time{}); err != nil {
			b.Fatal(err)
		}
	}
}
