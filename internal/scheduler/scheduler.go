package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/minicron/minicron/internal/config"
	"github.com/minicron/minicron/internal/executor"
	"github.com/minicron/minicron/internal/model"
	"github.com/minicron/minicron/internal/store"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type Scheduler struct {
	store  *store.Store
	exec   *executor.Service
	mu     sync.Mutex
	cancel context.CancelFunc
	defs   map[string]model.Definition
}

func New(st *store.Store, ex *executor.Service) *Scheduler {
	return &Scheduler{store: st, exec: ex, defs: make(map[string]model.Definition)}
}
func (s *Scheduler) Reload(ctx context.Context, defs []model.Definition) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.defs = make(map[string]model.Definition)
	for _, d := range defs {
		if d.Kind == model.KindJob && d.IsEnabled() && (d.Schedule != "") {
			s.defs[d.Name] = d
			go s.loop(runCtx, d)
		}
	}
	s.mu.Unlock()
	return nil
}
func (s *Scheduler) loop(ctx context.Context, d model.Definition) {
	hashBytes := sha256.Sum256([]byte(d.Schedule + "\x00" + d.Timezone))
	hash := hex.EncodeToString(hashBytes[:])
	anchor, last, storedHash, err := s.store.ScheduleState(ctx, d.ID)
	now := time.Now().UTC()
	if err != nil || storedHash != hash {
		anchor = now
		last = time.Time{}
		s.store.SetScheduleState(ctx, d.ID, hash, anchor, time.Time{})
	}
	if !last.IsZero() && last.Before(now) {
		next, err := nextFire(d, last, anchor)
		if err == nil && next.Before(now) {
			count := 0
			cursor := next
			latest := next
			for !cursor.After(now) && count < 10000 {
				latest = cursor
				count++
				cursor, _ = nextFire(d, cursor, anchor)
			}
			if count > 0 {
				if d.CatchUp == "latest" {
					scheduled := latest
					s.exec.Trigger(context.Background(), d, mustHash(d), "schedule", &scheduled)
				} else {
					s.exec.RecordMissed(context.Background(), d, mustHash(d), count, latest)
				}
				last = latest
				s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last)
			}
		}
	}
	for {
		next, err := nextFire(d, maxTime(last, time.Now().UTC()), anchor)
		if err != nil {
			return
		}
		// Keep the pending fire in durable state so API clients can display
		// the same instant the scheduler is waiting for.
		_ = s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, next)
		for time.Now().Before(next) {
			wait := min(time.Until(next), 30*time.Second)
			timer := time.NewTimer(max(wait, 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// Recompute from wall time after each bounded monotonic wait only
			// while the pending slot is still ahead; nextFire is strictly after
			// its input, so recomputing once now has reached `next` would defer
			// that occurrence by another interval — forever.
			now := time.Now().UTC()
			if !now.Before(next) {
				break
			}
			recomputed, err := nextFire(d, maxTime(last, now), anchor)
			if err != nil {
				return
			}
			next = recomputed
			_ = s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, next)
		}
		scheduled := next
		_, _ = s.exec.Trigger(context.Background(), d, mustHash(d), "schedule", &scheduled)
		last = next

		// Publish the following fire immediately after triggering this one.
		// This prevents the API from reporting the just-fired instant while
		// the next scheduler iteration is being prepared.
		following, err := nextFire(d, maxTime(last, time.Now().UTC()), anchor)
		if err != nil {
			_ = s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last)
			return
		}
		_ = s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, following)
	}
}
func nextFire(d model.Definition, after, anchor time.Time) (time.Time, error) {
	if raw, ok := strings.CutPrefix(d.Schedule, "@every "); ok {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return time.Time{}, err
		}
		if after.Before(anchor) {
			return anchor.Add(interval), nil
		}
		steps := after.Sub(anchor)/interval + 1
		return anchor.Add(steps * interval), nil
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	schedule, err := parser.Parse(d.Schedule)
	if err != nil {
		return time.Time{}, err
	}
	candidate := schedule.Next(after.In(loc)).UTC()

	// Suppress the second instance of a wall-clock minute in a DST fold.
	if sameWallMinute(after.In(loc), candidate.In(loc)) {
		candidate = schedule.Next(candidate.In(loc)).UTC()
	}

	// robfig/cron correctly skips nonexistent wall times. minicron's contract
	// instead coalesces any matching minute in a forward gap at the transition.
	if transition, oldOffset, newOffset, ok := forwardTransition(after, candidate, loc); ok {
		fixed := time.FixedZone("before-dst", oldOffset)
		gapStart := transition.In(fixed).Truncate(time.Minute)
		for minute := gapStart; minute.Before(gapStart.Add(time.Duration(newOffset-oldOffset) * time.Second)); minute = minute.Add(time.Minute) {
			if schedule.Next(minute.Add(-time.Minute)).Equal(minute) {
				return transition.UTC(), nil
			}
		}
	}
	return candidate, nil
}

func sameWallMinute(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd && a.Hour() == b.Hour() && a.Minute() == b.Minute()
}

func forwardTransition(start, end time.Time, loc *time.Location) (time.Time, int, int, bool) {
	if !end.After(start) {
		return time.Time{}, 0, 0, false
	}
	cursor := start
	_, previous := cursor.In(loc).Zone()
	for cursor.Before(end) {
		next := minTime(cursor.Add(6*time.Hour), end)
		_, offset := next.In(loc).Zone()
		if offset != previous {
			low, high := cursor, next
			for high.Sub(low) > time.Second {
				mid := low.Add(high.Sub(low) / 2)
				_, atMid := mid.In(loc).Zone()
				if atMid == previous {
					low = mid
				} else {
					high = mid
				}
			}
			transition := high.Truncate(time.Second)
			if offset > previous {
				return transition, previous, offset, true
			}
			previous = offset
		}
		cursor = next
	}
	return time.Time{}, 0, 0, false
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
func mustHash(d model.Definition) string {
	_, h, err := config.Canonical(d)
	if err != nil {
		panic(fmt.Sprintf("canonical definition: %v", err))
	}
	return h
}
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}
