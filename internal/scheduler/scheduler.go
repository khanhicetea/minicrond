package scheduler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/khanhicetea/minicrond/internal/config"
	"github.com/khanhicetea/minicrond/internal/executor"
	"github.com/khanhicetea/minicrond/internal/fault"
	"github.com/khanhicetea/minicrond/internal/model"
	"github.com/khanhicetea/minicrond/internal/store"
)

var parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type Scheduler struct {
	store   *store.Store
	exec    *executor.Service
	mu      sync.Mutex
	loops   sync.WaitGroup
	running map[string]scheduledLoop
	stopped bool // set by Stop; Reload then refuses to start loops
}

type scheduledLoop struct {
	def    model.Definition
	cancel context.CancelFunc
	done   chan struct{}
}

// ErrStopped is returned by Reload after Stop.
var ErrStopped = errors.New("scheduler stopped")

func New(st *store.Store, ex *executor.Service) *Scheduler {
	return &Scheduler{store: st, exec: ex, running: make(map[string]scheduledLoop)}
}
func (s *Scheduler) Reload(ctx context.Context, defs []model.Definition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.stopped {
		return ErrStopped
	}
	// Keep indexes into defs: copying these large definitions into map values
	// allocates once per job, even when every scheduling loop is unchanged.
	wanted := make(map[string]int, len(defs))
	for i := range defs {
		d := &defs[i]
		if d.Kind == model.KindJob && d.IsEnabled() && (d.Schedule != "") {
			wanted[d.Name] = i
		}
	}
	// Join changed loops before their replacements start. This preserves the
	// one-loop-per-job rule near a scheduled fire.
	for name, old := range s.running {
		if i, ok := wanted[name]; ok && sameDefinition(&old.def, &defs[i]) {
			select {
			case <-old.done: // An exited loop needs a fresh attempt.
			default:
				continue
			}
		}
		old.cancel()
		<-old.done
		delete(s.running, name)
	}
	for i := range defs {
		index, ok := wanted[defs[i].Name]
		if !ok {
			continue
		}
		if _, ok := s.running[defs[index].Name]; ok {
			continue
		}
		d := defs[index].Clone()
		runCtx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		s.running[d.Name] = scheduledLoop{def: d, cancel: cancel, done: done}
		s.loops.Go(func() {
			defer close(done)
			defer cancel()
			if err := fault.Call(func() error {
				s.loop(runCtx, d)
				return nil
			}); err != nil {
				if panicErr, ok := errors.AsType[*fault.PanicError](err); ok {
					slog.Error("scheduler loop panicked", "job", d.Name, "error", err, "stack", string(panicErr.Stack))
				}
			}
		})
	}
	return nil
}

// loop restarts a failed scheduling pass with capped exponential backoff.
// A storage failure must not stop a job's schedule until the next reload.
// Each pass reloads the persisted watermark, so an occurrence that could not
// be triggered is handled by the definition's catch_up policy on restart.
func (s *Scheduler) loop(ctx context.Context, d model.Definition) {
	const maxBackoff = time.Minute
	backoff := time.Second
	for {
		started := time.Now()
		err := s.runPass(ctx, d)
		if err == nil || ctx.Err() != nil {
			return
		}
		if time.Since(started) > maxBackoff {
			backoff = time.Second
		}
		slog.Error("scheduler: scheduling pass failed; retrying", "job", d.Name, "retry_in", backoff, "error", err)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// runPass schedules d until ctx ends or a step fails. It returns nil when the
// loop should stop for good (cancellation or an unusable schedule) and an
// error when a retry may succeed.
func (s *Scheduler) runPass(ctx context.Context, d model.Definition) error {
	if ctx.Err() != nil {
		return nil
	}
	// A panic here would take the whole daemon down; definitions are
	// validated before they reach the scheduler, so a canonicalization
	// failure logs and drops the loop instead.
	_, defHash, err := config.Canonical(d)
	if err != nil {
		slog.Error("scheduler: canonicalizing definition failed", "job", d.Name, "error", err)
		return nil
	}
	schedule, err := compileSchedule(d)
	if err != nil {
		slog.Error("scheduler: compiling schedule failed", "job", d.Name, "error", err)
		return nil
	}
	hashBytes := sha256.Sum256([]byte(d.Schedule + "\x00" + d.Timezone))
	hash := hex.EncodeToString(hashBytes[:])
	anchor, last, persistedNext, storedHash, err := s.store.ScheduleState(ctx, d.ID)
	if ctx.Err() != nil {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read schedule state: %w", err)
	}
	persistedLast := last
	hasPersisted := err == nil && storedHash == hash
	now := time.Now().UTC()
	if err != nil || storedHash != hash {
		anchor = now
		last = time.Time{}
		if err := s.store.SetScheduleState(ctx, d.ID, hash, anchor, time.Time{}); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("initialize schedule state: %w", err)
		}
		persistedLast, persistedNext, hasPersisted = last, time.Time{}, true
	}
	// Only a successful database write advances the cached state. A reload can
	// start from the next fire already stored by the previous loop.
	persistNext := func(next time.Time) error {
		if hasPersisted && persistedLast.Equal(last) && persistedNext.Equal(next) {
			return nil
		}
		if err := s.store.SetScheduleStateWithNext(context.Background(), d.ID, hash, anchor, last, next); err != nil {
			return err
		}
		persistedLast, persistedNext, hasPersisted = last, next, true
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	if !last.IsZero() && last.Before(now) {
		next, err := schedule.nextFireDistinct(last, anchor, last)
		if err != nil {
			slog.Error("scheduler: calculating initial catch-up fire failed", "job", d.Name, "error", err)
			return nil
		}
		if next.Before(now) {
			count := 0
			cursor := next
			latest := next
			if schedule.interval > 0 {
				steps := now.Sub(next) / schedule.interval
				latest = next.Add(steps * schedule.interval)
				// Saturate before the addition and conversion. Nanosecond
				// intervals can exceed an int's count over a long downtime.
				if steps >= time.Duration(math.MaxInt) {
					count = math.MaxInt
				} else {
					count = int(steps) + 1
				}
			} else {
				for !cursor.After(now) && count < 10000 {
					latest = cursor
					count++
					following, nextErr := schedule.nextFireDistinct(cursor, anchor, latest)
					if nextErr != nil {
						slog.Error("scheduler: calculating catch-up fire failed", "job", d.Name, "error", nextErr)
						break
					}
					cursor = following
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
						return nil
					}
					return fmt.Errorf("catch up: %w", err)
				}
				last = latest
				if stateErr := s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last); stateErr != nil {
					// The run row is unique per occurrence, so retrying catch-up
					// after this failure cannot fire the occurrence twice.
					return fmt.Errorf("persist catch-up watermark: %w", stateErr)
				}
				persistedLast, persistedNext = last, time.Time{}
			}
		}
	}
	for ctx.Err() == nil {
		next, err := schedule.nextFireDistinct(maxTime(last, time.Now().UTC()), anchor, last)
		if err != nil {
			slog.Error("scheduler: calculating next fire failed", "job", d.Name, "error", err)
			return nil
		}
		// Keep the pending fire in durable state so API clients can display
		// the same instant the scheduler is waiting for.
		if err := persistNext(next); err != nil {
			return fmt.Errorf("persist next fire: %w", err)
		}
		for time.Now().Before(next) {
			wait := min(time.Until(next), 30*time.Second)
			timer := time.NewTimer(max(wait, 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
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
			recomputed, err := schedule.nextFireDistinct(maxTime(last, now), anchor, last)
			if err != nil {
				slog.Error("scheduler: recalculating next fire failed", "job", d.Name, "error", err)
				return nil
			}
			next = recomputed
			if err := persistNext(next); err != nil {
				return fmt.Errorf("persist recomputed fire: %w", err)
			}
		}
		scheduled := next
		if _, err := s.exec.Trigger(ctx, d, defHash, "schedule", &scheduled); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// The watermark still precedes this occurrence, so the next pass
			// applies catch_up to it instead of silently dropping it.
			return fmt.Errorf("trigger occurrence %s: %w", scheduled.Format(time.RFC3339), err)
		}
		last = next

		// Publish the following fire immediately after triggering this one.
		// This prevents the API from reporting the just-fired instant while
		// the next scheduler iteration is being prepared.
		following, err := schedule.nextFireDistinct(maxTime(last, time.Now().UTC()), anchor, last)
		if err != nil {
			slog.Error("scheduler: calculating following fire failed", "job", d.Name, "error", err)
			if stateErr := s.store.SetScheduleState(context.Background(), d.ID, hash, anchor, last); stateErr != nil {
				return fmt.Errorf("persist watermark: %w", stateErr)
			}
			return nil
		}
		if err := persistNext(following); err != nil {
			return fmt.Errorf("persist following fire: %w", err)
		}
	}
	return nil
}

type compiledSchedule struct {
	raw      string
	interval time.Duration
	loc      *time.Location
	cron     cron.Schedule
}

func compileSchedule(d model.Definition) (compiledSchedule, error) {
	c := compiledSchedule{raw: d.Schedule}
	if raw, ok := strings.CutPrefix(d.Schedule, "@every "); ok {
		interval, err := time.ParseDuration(raw)
		if err != nil {
			return c, err
		}
		if interval <= 0 {
			return c, fmt.Errorf("schedule interval must be positive: %q", raw)
		}
		c.interval = interval
		return c, nil
	}
	loc, err := time.LoadLocation(d.Timezone)
	if err != nil {
		return c, err
	}
	schedule, err := parser.Parse(d.Schedule)
	if err != nil {
		return c, err
	}
	c.loc, c.cron = loc, schedule
	return c, nil
}

func nextFireDistinct(d model.Definition, after, anchor, last time.Time) (time.Time, error) {
	c, err := compileSchedule(d)
	if err != nil {
		return time.Time{}, err
	}
	return c.nextFireDistinct(after, anchor, last)
}

func (c compiledSchedule) nextFireDistinct(after, anchor, last time.Time) (time.Time, error) {
	candidate, err := c.nextFire(after, anchor)
	if err != nil || last.IsZero() || c.interval > 0 {
		return candidate, err
	}
	if sameWallMinute(last.In(c.loc), candidate.In(c.loc)) {
		return c.nextFire(candidate, anchor)
	}
	return candidate, nil
}

func nextFire(d model.Definition, after, anchor time.Time) (time.Time, error) {
	c, err := compileSchedule(d)
	if err != nil {
		return time.Time{}, err
	}
	return c.nextFire(after, anchor)
}

func (c compiledSchedule) nextFire(after, anchor time.Time) (time.Time, error) {
	if c.interval > 0 {
		if after.Before(anchor) {
			return anchor.Add(c.interval), nil
		}
		periods := after.Sub(anchor) / c.interval
		if periods >= time.Duration(math.MaxInt64)/c.interval {
			return time.Time{}, fmt.Errorf("schedule interval exceeds supported time range: %q", c.raw)
		}
		candidate := anchor.Add((periods + 1) * c.interval)
		if !candidate.After(after) {
			return time.Time{}, fmt.Errorf("schedule does not advance beyond current time: %q", c.raw)
		}
		return candidate, nil
	}
	candidate := c.cron.Next(after.In(c.loc)).UTC()
	if candidate.IsZero() {
		return time.Time{}, fmt.Errorf("schedule has no future occurrence: %q", c.raw)
	}

	// Suppress the second instance of a wall-clock minute in a DST fold.
	if sameWallMinute(after.In(c.loc), candidate.In(c.loc)) {
		candidate = c.cron.Next(candidate.In(c.loc)).UTC()
	}

	// robfig/cron correctly skips nonexistent wall times. minicron's contract
	// instead coalesces any matching minute in a forward gap at the transition.
	if transition, oldOffset, newOffset, ok := forwardTransition(after, candidate, c.loc); ok {
		fixed := time.FixedZone("before-dst", oldOffset)
		gapStart := transition.In(fixed).Truncate(time.Minute)
		gapEnd := gapStart.Add(time.Duration(newOffset-oldOffset) * time.Second)
		firstInGap := c.cron.Next(gapStart.Add(-time.Minute))
		if !firstInGap.IsZero() && firstInGap.Before(gapEnd) {
			return transition.UTC(), nil
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
	cursor := start.In(loc)
	_, previous := cursor.In(loc).Zone()
	for cursor.Before(end) {
		// ZoneBounds jumps directly to the next location transition. Fixed
		// zones return a zero end, so distant cron fires cost one lookup.
		_, next := cursor.ZoneBounds()
		if next.IsZero() || next.After(end) || !next.After(cursor) {
			return time.Time{}, 0, 0, false
		}
		_, offset := next.In(loc).Zone()
		if offset > previous {
			return next.UTC(), previous, offset, true
		}
		previous = offset
		cursor = next
	}
	return time.Time{}, 0, 0, false
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
	s.stopped = true
	for _, loop := range s.running {
		loop.cancel()
	}
	s.loops.Wait()
	clear(s.running)
}
