package main

import (
	"fmt"
	"time"
)

// Shanghai is the only time zone the product serves; dates and clock times from the
// mini program are interpreted here.
var Shanghai = mustLoadLocation("Asia/Shanghai")

func mustLoadLocation(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

type Slot struct {
	Start    time.Time
	Duration time.Duration
}

const maxEvery = 15

// BuildSlots expands "date at clock, every N days, over a span of days": every 1 is the common
// daily holiday booking, every 3 the cat that only needs a visit every third day. The span
// counts from the first day to the last possible one, so 10 days every 3 gives days 1, 4, 7, 10.
func BuildSlots(date, clock string, durationMin, days, every int) ([]Slot, error) {
	if durationMin <= 0 || durationMin > 24*60 {
		return nil, fmt.Errorf("时长要在 1 到 1440 分钟之间")
	}
	if days <= 0 || days > 60 {
		return nil, fmt.Errorf("天数要在 1 到 60 之间")
	}
	if every == 0 {
		every = 1
	}
	if every < 1 || every > maxEvery {
		return nil, fmt.Errorf("间隔要在 1 到 %d 天之间", maxEvery)
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, Shanghai)
	if err != nil {
		return nil, fmt.Errorf("日期或时间格式不对")
	}
	var slots []Slot
	for i := 0; i < days; i += every {
		slots = append(slots, Slot{Start: start.AddDate(0, 0, i), Duration: time.Duration(durationMin) * time.Minute})
	}
	return slots, nil
}

type Conflict struct {
	Start      int64  `json:"start_at"`
	ClientName string `json:"client_name"`
	ExistingAt int64  `json:"existing_start_at"`
}

// FindConflicts reports new slots that overlap an existing visit once travel time is added
// on both sides: two visits clash when either cannot be reached from the other in time.
func FindConflicts(slots []Slot, existing []Visit, travel time.Duration) []Conflict {
	var out []Conflict
	for _, s := range slots {
		sEnd := s.Start.Add(s.Duration)
		for _, v := range existing {
			vStart := time.Unix(v.StartAt, 0)
			vEnd := vStart.Add(time.Duration(v.DurationMin) * time.Minute)
			if s.Start.Before(vEnd.Add(travel)) && vStart.Before(sEnd.Add(travel)) {
				out = append(out, Conflict{Start: s.Start.Unix(), ClientName: v.ClientName, ExistingAt: v.StartAt})
			}
		}
	}
	return out
}
