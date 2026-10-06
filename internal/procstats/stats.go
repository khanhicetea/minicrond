// Package procstats provides on-demand direct-process statistics.
package procstats

import "time"

type Sample struct {
	SampledAt time.Time `json:"sampled_at"`
	StartID   string    `json:"process_start_id"`
	CPUUS     int64     `json:"cpu_us"`
	RSSBytes  int64     `json:"rss_bytes"`
}
