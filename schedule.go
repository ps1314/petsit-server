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

// BuildSlots expands "date at clock for N consecutive days", the common shape of a holiday booking.
func BuildSlots(date, clock string, durationMin, days int) ([]Slot, error) {
	if durationMin <= 0 || durationMin > 24*60 {
		return nil, fmt.Errorf("时长要在 1 到 1440 分钟之间")
	}
	if days <= 0 || days > 60 {
		return nil, fmt.Errorf("连续天数要在 1 到 60 之间")
	}
	start, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, Shanghai)
	if err != nil {
		return nil, fmt.Errorf("日期或时间格式不对")
	}
	slots := make([]Slot, days)
	for i := range slots {
		slots[i] = Slot{Start: start.AddDate(0, 0, i), Duration: time.Duration(durationMin) * time.Minute}
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
