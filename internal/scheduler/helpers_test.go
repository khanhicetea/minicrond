package scheduler

import (
	"time"

	"github.com/khanhicetea/minicrond/internal/model"
)

func nextFire(d model.Definition, after, anchor time.Time) (time.Time, error) {
	c, err := compileSchedule(d)
	if err != nil {
		return time.Time{}, err
	}
	return c.nextFire(after, anchor)
}
