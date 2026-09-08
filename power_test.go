package main

import (
	"encoding/json"
	"testing"
	"time"

	weenect "github.com/perbu/weenect-go"
)

// mytrackerItem is the shape of one GET /mytracker item, trimmed to the
// fields power.go decodes. The nulls and the single-element position array
// mirror what the live API sends.
const mytrackerItem = `{
  "id": 616818,
  "name": "Bella",
  "active": true,
  "remaining_days": 1,
  "position": [{
    "battery": 53,
    "date_server": "2026-09-08T15:13:49+00:00",
    "date_tracker": "2026-09-08T15:07:49+00:00",
    "last_message": null,
    "is_online": true,
    "is_in_deep_sleep": false,
    "off_reason": null
  }]
}`

func TestWeenectTrackerStateDecodesLivePayload(t *testing.T) {
	var item weenectTrackerState
	if err := json.Unmarshal([]byte(mytrackerItem), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	now := time.Date(2026, 9, 8, 15, 15, 0, 0, time.UTC)
	got := item.state(now)

	if got.Power != PowerLive {
		t.Errorf("Power = %q, want %q", got.Power, PowerLive)
	}
	if got.OffReason != nil {
		t.Errorf("OffReason = %q, want nil", *got.OffReason)
	}
	// last_message is null, so the fix time falls through to date_server.
	want := time.Date(2026, 9, 8, 15, 13, 49, 0, time.UTC)
	if !got.LastFixAt.Equal(want) {
		t.Errorf("LastFixAt = %v, want %v", got.LastFixAt, want)
	}
	if !got.SubscriptionActive || got.RemainingDays != 1 {
		t.Errorf("subscription = (%v, %d days), want (true, 1 day)", got.SubscriptionActive, got.RemainingDays)
	}
}

func TestTrackerStateClassification(t *testing.T) {
	reason := "ALM-OFF"
	fix := time.Date(2026, 9, 7, 17, 7, 31, 0, time.UTC)
	now := fix.Add(time.Minute)

	cases := []struct {
		name string
		in   weenectTrackerState
		want DevicePower
	}{
		{
			name: "switched off names its reason",
			in: weenectTrackerState{Active: true, Position: []weenectStatePosition{{
				DateServer: wt(fix), OffReason: &reason,
			}}},
			want: PowerOff,
		},
		{
			name: "offline without a reason is not called off",
			in: weenectTrackerState{Active: true, Position: []weenectStatePosition{{
				DateServer: wt(fix),
			}}},
			want: PowerOffline,
		},
		{
			// Deep sleep outranks is_online: reporting is suspended either way,
			// and sleeping says why.
			name: "deep sleep wins over online",
			in: weenectTrackerState{Active: true, Position: []weenectStatePosition{{
				DateServer: wt(fix), IsOnline: true, IsInDeepSleep: true,
			}}},
			want: PowerSleeping,
		},
		{
			// An inactive subscription silences the collar regardless of power,
			// so it takes precedence over whatever the last fix claimed.
			name: "lapsed subscription outranks a live fix",
			in: weenectTrackerState{Active: false, Position: []weenectStatePosition{{
				DateServer: wt(fix), IsOnline: true,
			}}},
			want: PowerExpired,
		},
		{
			// remaining_days hits zero on a renewal day; only the active flag
			// decides expiry, or a working collar would blank out.
			name: "zero remaining days on an active subscription stays live",
			in: weenectTrackerState{Active: true, RemainingDays: 0, Position: []weenectStatePosition{{
				DateServer: wt(fix), IsOnline: true,
			}}},
			want: PowerLive,
		},
		{
			name: "a tracker that never reported is unknown",
			in:   weenectTrackerState{Active: true},
			want: PowerUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.state(now).Power; got != tc.want {
				t.Errorf("Power = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReportedDowngradesStaleLive(t *testing.T) {
	fix := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	live := TrackerState{Power: PowerLive, LastFixAt: fix, CheckedAt: fix}

	if got := live.Reported(fix.Add(powerStaleAfter - time.Minute)).Power; got != PowerLive {
		t.Errorf("within the stale window: Power = %q, want %q", got, PowerLive)
	}
	if got := live.Reported(fix.Add(powerStaleAfter + time.Minute)).Power; got != PowerOffline {
		t.Errorf("past the stale window: Power = %q, want %q", got, PowerOffline)
	}

	// A state with no fix time has nothing to age against, and one that is
	// already off must not be relabelled as merely offline.
	noFix := TrackerState{Power: PowerLive, CheckedAt: fix}
	if got := noFix.Reported(fix.Add(24 * time.Hour)).Power; got != PowerLive {
		t.Errorf("without a fix time: Power = %q, want %q", got, PowerLive)
	}
	off := TrackerState{Power: PowerOff, LastFixAt: fix, CheckedAt: fix}
	if got := off.Reported(fix.Add(24 * time.Hour)).Power; got != PowerOff {
		t.Errorf("already off: Power = %q, want %q", got, PowerOff)
	}
}

// wt wraps a time as the API's WeenectTime for table-driven cases.
func wt(t time.Time) weenect.WeenectTime {
	return weenect.WeenectTime{Time: t}
}
