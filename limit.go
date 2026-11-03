package main

import (
	"sync"
	"time"
)

// DailyLimit counts actions per sitter per Shanghai calendar day. It lives in memory, so with
// several cloud hosting instances the effective cap is per instance; that is enough to stop
// one account from running up the model bill.
type DailyLimit struct {
	Max int
	now func() time.Time

	mu    sync.Mutex
	day   string
	count map[int64]int
}

func NewDailyLimit(max int) *DailyLimit {
	return &DailyLimit{Max: max, now: time.Now, count: map[int64]int{}}
}

func (l *DailyLimit) Allow(sitterID int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if day := l.now().In(Shanghai).Format("2006-01-02"); day != l.day {
		l.day, l.count = day, map[int64]int{}
	}
	if l.count[sitterID] >= l.Max {
		return false
	}
	l.count[sitterID]++
	return true
}
