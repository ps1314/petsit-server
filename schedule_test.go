package main

import (
	"testing"
	"time"
)

func TestBuildSlotsConsecutiveDays(t *testing.T) {
	slots, err := BuildSlots("2027-02-05", "09:30", 40, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 3 {
		t.Fatalf("got %d slots", len(slots))
	}
	if got := slots[2].Start.Format("2006-01-02 15:04 -0700"); got != "2027-02-07 09:30 +0800" {
		t.Fatalf("third slot = %s", got)
	}
}

func TestBuildSlotsRejectsBadInput(t *testing.T) {
	for _, c := range []struct {
		date, clock string
		dur, days   int
	}{
		{"2027-02-05", "09:30", 0, 1},
		{"2027-02-05", "09:30", 40, 0},
		{"2027-02-05", "9点半", 40, 1},
		{"2027/02/05", "09:30", 40, 1},
	} {
		if _, err := BuildSlots(c.date, c.clock, c.dur, c.days); err == nil {
			t.Errorf("%+v: expected error", c)
		}
	}
}

func TestFindConflictsUsesTravelBuffer(t *testing.T) {
	at := func(clock string) int64 {
		tm, _ := time.ParseInLocation("2006-01-02 15:04", "2027-02-05 "+clock, Shanghai)
		return tm.Unix()
	}
	existing := []Visit{{StartAt: at("09:00"), DurationMin: 40, ClientName: "王姐"}} // ends 09:40
	cases := []struct {
		clock string
		clash bool
	}{
		{"10:10", false}, // 09:40 + 30 min travel = 10:10, just reachable
		{"10:05", true},
		{"08:00", true},  // 08:40 + 30 = 09:10 > 09:00
		{"07:50", false}, // 08:30 + 30 = 09:00
	}
	for _, c := range cases {
		slots, _ := BuildSlots("2027-02-05", c.clock, 40, 1)
		got := FindConflicts(slots, existing, 30*time.Minute)
		if (len(got) > 0) != c.clash {
			t.Errorf("%s: clash=%v, want %v", c.clock, len(got) > 0, c.clash)
		}
		if c.clash && got[0].ClientName != "王姐" {
			t.Errorf("%s: conflict names %q", c.clock, got[0].ClientName)
		}
	}
}
