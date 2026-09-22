package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type Scheduler struct {
	store  *store.Store
	exec   *executor.Service
	mu     sync.Mutex
	cancel context.CancelFunc
	loops  sync.WaitGroup
	defs   map[string]model.Definition
}

func New(st *store.Store, ex *executor.Service) *Scheduler {
	return &Scheduler{store: st, exec: ex, defs: make(map[string]model.Definition)}
}
func (s *Scheduler) Reload(ctx context.Context, defs []model.Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.cancel != nil {
		s.cancel()
		s.loops.Wait()
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.defs = make(map[string]model.Definition)
	for _, d := range defs {
		if d.Kind == model.KindJob && d.IsEnabled() && (d.Schedule != "") {
			s.defs[d.Name] = d
			s.loops.Go(func() {
				s.loop(runCtx, d)
			})
		}
	}
	return nil
}
func (s *Scheduler) loop(ctx context.Context, d model.Definition) {
	// A panic here would take the whole daemon down; definitions are
	// validated before they reach the scheduler, so a canonicalization
	// failure logs and drops the loop instead.
	_, defHash, err := config.Canonical(d)
	if err != nil {
		slog.Error("scheduler: canonicalizing definition failed", "job", d.Name, "error", err)
		return
	}
	hashBytes := sha256.Sum256([]byte(d.Schedule + "\x00" + d.Timezone))
	hash := hex.EncodeToString(hashBytes[:])
	anchor, last, storedHash, err := s.store.ScheduleState(ctx, d.ID)
	now := time.Now().UTC()
	if err != nil || storedHash != hash {
		anchor = now
		last = time.Time{}
		if err := s.store.SetScheduleState(ctx, d.ID, hash, anchor, time.Time{}); err != nil {
			slog.Error("scheduler: initialize state failed", "job", d.Name, "error", err)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	if !last.IsZero() && last.Before(now) {
		next, err := nextFireDistinct(d, last, anchor, last)
		if err == nil && next.Before(now) {
			count := 0
			cursor := next
			latest := next
			if raw, ok := strings.CutPrefix(d.Schedule, "@every "); ok {
				interval, parseErr := time.ParseDuration(raw)
				if parseErr != nil || interval <= 0 {
					return
				}
				steps := now.Sub(next) / interval
				latest = next.Add(steps * interval)
				count = int(steps + 1)
			} else {
				for !cursor.After(now) && count < 10000 {
					latest = cursor
					count++
					cursor, _ = nextFireDistinct(d, cursor, anchor, latest)
				}
				if count == 10000 && !cursor.After(now) {
					slog.Warn("scheduler: cron catch-up summarized at safety bound", "job", d.Name, "count", count)
				}
			}
			if count > 0 {
				if d.CatchUp == "latest" {
					scheduled := latest
					_, err = s.exec.Trigger(ctx, d, defHash, "schedule", &scheduled)
				} else {
					_, err = s.exec.RecordMissed(ctx, d, defHash, count, latest)
				}
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Error("scheduler: catch-up failed", "job", d.Name, "error", err)
					return
				}
				last = latest
				if stateErr := s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last); stateErr != nil {
					slog.Error("scheduler: persist catch-up watermark failed", "job", d.Name, "error", stateErr)
					return
				}
			}
		}
	}
	for ctx.Err() == nil {
		next, err := nextFireDistinct(d, maxTime(last, time.Now().UTC()), anchor, last)
		if err != nil {
			return
		}
		// Keep the pending fire in durable state so API clients can display
		// the same instant the scheduler is waiting for.
		if err := s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, next); err != nil {
			slog.Error("scheduler: persist next fire failed", "job", d.Name, "error", err)
			return
		}
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
			recomputed, err := nextFireDistinct(d, maxTime(last, now), anchor, last)
			if err != nil {
				return
			}
			next = recomputed
			if err := s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, next); err != nil {
				slog.Error("scheduler: persist recomputed fire failed", "job", d.Name, "error", err)
				return
			}
		}
		scheduled := next
		if _, err := s.exec.Trigger(ctx, d, defHash, "schedule", &scheduled); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Error("scheduler: trigger failed", "job", d.Name, "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		last = next

		// Publish the following fire immediately after triggering this one.
		// This prevents the API from reporting the just-fired instant while
		// the next scheduler iteration is being prepared.
		following, err := nextFireDistinct(d, maxTime(last, time.Now().UTC()), anchor, last)
		if err != nil {
			if stateErr := s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last); stateErr != nil {
				slog.Error("scheduler: persist watermark failed", "job", d.Name, "error", stateErr)
			}
			return
		}
		if err := s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, following); err != nil {
			slog.Error("scheduler: persist following fire failed", "job", d.Name, "error", err)
			return
		}
	}
}
func nextFireDistinct(d model.Definition, after, anchor, last time.Time) (time.Time, error) {
	candidate, err := nextFire(d, after, anchor)
	if err != nil || last.IsZero() || strings.HasPrefix(d.Schedule, "@every ") {
		return candidate, err
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	if sameWallMinute(last.In(loc), candidate.In(loc)) {
		return nextFire(d, candidate, anchor)
	}
	return candidate, nil
}

func nextFire(d model.Definition, after, anchor time.Time) (time.Time, error) {
	if raw, ok := strings.CutPrefix(d.Schedule, "@every "); ok {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return time.Time{}, err
		}
		if interval <= 0 {
			return time.Time{}, fmt.Errorf("schedule interval must be positive: %q", raw)
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
	if candidate.IsZero() {
		return time.Time{}, fmt.Errorf("schedule has no future occurrence: %q", d.Schedule)
	}

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

// Stop cancels all scheduling loops and waits until they can no longer fire.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.loops.Wait()
	}
}
