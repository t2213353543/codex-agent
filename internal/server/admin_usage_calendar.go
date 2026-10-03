package server

import (
	"fmt"
	"time"
)

func validManagedUsageStep(step int64) bool {
	return step == 60 || step == 300 || step == 3600 || step == 86400 || step == 604800 || step == 2592000
}
func managedUsageBucket(stamp, step int64, location *time.Location) int64 {
	if step < 86400 {
		return stamp / step * step
	}
	t := time.Unix(stamp, 0).In(location)
	year, month, day := t.Date()
	if step == 2592000 {
		day = 1
	}
	bucket := time.Date(year, month, day, 0, 0, 0, 0, location)
	if step == 604800 {
		days := (int(t.Weekday()) + 6) % 7
		bucket = bucket.AddDate(0, 0, -days)
	}
	return bucket.Unix()
}

// Fill absent buckets only inside an explicit selection (or actual data span).
// Calendar increments preserve 23/25-hour days and variable-length months.
func (out *managedUsageStats) fillSeries() error {
	location, err := time.LoadLocation(out.Timezone)
	if err != nil {
		return errInvalid
	}
	from, to := out.From, out.To
	if from == 0 {
		from = out.FirstRecordAt
	}
	if to == 0 {
		to = out.CheckedAt
		if out.From == 0 {
			to = out.LastRecordAt + 1
		}
	}
	if from == 0 || to <= from {
		return nil
	}
	byTime := map[int64]managedUsagePoint{}
	for _, point := range out.Series {
		byTime[point.Time] = point
	}
	filled := []managedUsagePoint{}
	for stamp := managedUsageBucket(from, out.Step, location); stamp < to; {
		if len(filled) >= 5000 {
			return fmt.Errorf("%w: narrow the range for this chart interval", errInvalid)
		}
		point, exists := byTime[stamp]
		if !exists {
			point = managedUsagePoint{Time: stamp}
		}
		filled = append(filled, point)
		if out.Step < 86400 {
			stamp += out.Step
			continue
		}
		date := time.Unix(stamp, 0).In(location)
		switch out.Step {
		case 86400:
			date = date.AddDate(0, 0, 1)
		case 604800:
			date = date.AddDate(0, 0, 7)
		case 2592000:
			date = date.AddDate(0, 1, 0)
		default:
			return errInvalid
		}
		if date.Unix() <= stamp {
			return errInvalid
		}
		stamp = date.Unix()
	}
	out.Series = filled
	return nil
}
