package main

import (
	"time"

	weenect "github.com/perbu/weenect-go"
)

// DevicePower is what the collar itself is doing, as reported by Weenect's
// tracker endpoint. It answers "is the tracker on?", which a gap in the
// position stream cannot: the collars come off most evenings, and a
// switched-off collar looks exactly like one that has lost GSM coverage or is
// simply between fixes.
type DevicePower string

const (
	// PowerLive means online and reporting.
	PowerLive DevicePower = "live"
	// PowerOff means the collar is off, and Weenect named the cause in
	// off_reason (ALM-OFF for the power button, a battery alarm for a flat
	// cell).
	PowerOff DevicePower = "off"
	// PowerOffline means unreachable with no cause given — out of GSM
	// coverage, or switched off so abruptly the alarm never got out.
	PowerOffline DevicePower = "offline"
	// PowerSleeping means deep sleep, so reporting is suspended by design.
	PowerSleeping DevicePower = "sleeping"
	// PowerExpired means the subscription has lapsed. The collar may well be
	// on, but no data arrives either way.
	PowerExpired DevicePower = "expired"
	// PowerUnknown means never polled: a fresh database, or Weenect
	// unreachable since startup. Never rendered as a claim about the device.
	PowerUnknown DevicePower = "unknown"
)

// powerStaleAfter is how old the newest upstream fix may get before a
// nominally-live collar is reported as stale instead. The collars run a 1
// minute reporting interval with a 5 minute keep-alive, so a quarter hour of
// silence from a tracker Weenect still calls online means one of the two is
// wrong — and saying so beats presenting the last fix as current.
const powerStaleAfter = 15 * time.Minute

// TrackerState is a collar's reporting state as of the last poll of Weenect's
// tracker endpoint. It is stored per tracker so the state survives restarts
// and can be served without a fresh upstream call.
type TrackerState struct {
	Power     DevicePower
	OffReason *string
	// SubscriptionActive and RemainingDays come from the tracker record, not
	// the collar: an expiring subscription silences a perfectly healthy
	// tracker, so it belongs in the same indicator.
	SubscriptionActive bool
	RemainingDays      int
	// LastFixAt is the timestamp of the newest fix Weenect holds, which is
	// when the collar last reported — the honest "since" for any state other
	// than live. Zero when unknown.
	LastFixAt time.Time
	// CheckedAt is when this state was polled. Zero means never.
	CheckedAt time.Time
}

// Reported returns the state with staleness overlaid, given the current time.
// Stored state is kept exactly as Weenect reported it; a live collar whose
// newest fix has gone quiet for powerStaleAfter is downgraded to
// PowerOffline here, at response time, since staleness depends on now and not
// on when the poll happened. An unpolled state is left alone.
func (s TrackerState) Reported(now time.Time) TrackerState {
	if s.Power != PowerLive || s.LastFixAt.IsZero() {
		return s
	}
	if now.Sub(s.LastFixAt) > powerStaleAfter {
		s.Power = PowerOffline
	}
	return s
}

// PowerStatus is the collar's reporting state as served to the UI: the polled
// state with staleness overlaid, plus the timestamps behind it, so a page can
// say how long a tracker has been off and how fresh that claim is rather than
// presenting the last known fix as current.
type PowerStatus struct {
	State     DevicePower `json:"state"`
	OffReason *string     `json:"off_reason,omitempty"`
	// LastFixAt is when the collar last reported to Weenect — the "since" for
	// any state other than live.
	LastFixAt *time.Time `json:"last_fix_at,omitempty"`
	// CheckedAt is when this state was polled; nil means never, which is the
	// only case where State is "unknown" for lack of data.
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	SubscriptionActive bool       `json:"subscription_active"`
	// RemainingDays counts down to the subscription's expiry. An expiring
	// subscription silences a working collar, so it belongs with the on/off
	// state. Nil until polled.
	RemainingDays *int `json:"remaining_days,omitempty"`
}

// newPowerStatus renders a stored tracker state for the API as of now.
// Returns nil when the tracker has never been polled, so clients can tell
// "no answer yet" from a claim about the device.
func newPowerStatus(s TrackerState, now time.Time) *PowerStatus {
	if s.CheckedAt.IsZero() {
		return nil
	}

	s = s.Reported(now)
	checked := s.CheckedAt
	days := s.RemainingDays
	p := &PowerStatus{
		State:              s.Power,
		OffReason:          s.OffReason,
		CheckedAt:          &checked,
		SubscriptionActive: s.SubscriptionActive,
		RemainingDays:      &days,
	}
	if !s.LastFixAt.IsZero() {
		fix := s.LastFixAt
		p.LastFixAt = &fix
	}
	return p
}

// weenectTrackerState is the slice of Weenect's GET /mytracker payload that
// cat2k needs. weenect-go's Tracker type carries only id and name, so the
// response is decoded from raw JSON (Client.DoRawRequest) instead of through
// Client.GetTrackers.
type weenectTrackerState struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	// RemainingDays counts down to expiration_date, which the API serves
	// without a timezone; the day count sidesteps that ambiguity.
	RemainingDays int `json:"remaining_days"`
	// Position holds the tracker's newest fix. Weenect wraps it in a
	// single-element array, and it is the only place the payload reports
	// is_online, off_reason and is_in_deep_sleep.
	Position []weenectStatePosition `json:"position"`
}

// weenectStatePosition is the embedded latest fix from the tracker endpoint,
// reduced to the fields that describe the device rather than its location.
type weenectStatePosition struct {
	IsOnline      bool    `json:"is_online"`
	OffReason     *string `json:"off_reason"`
	IsInDeepSleep bool    `json:"is_in_deep_sleep"`
	// Timestamps mirror the position endpoint's; weenect.WeenectTime tolerates
	// the nulls the API sends for last_message.
	LastMessage weenect.WeenectTime `json:"last_message"`
	DateServer  weenect.WeenectTime `json:"date_server"`
	DateTracker weenect.WeenectTime `json:"date_tracker"`
}

// timestamp mirrors weenect.Position.GetTimestamp's preference order.
func (p weenectStatePosition) timestamp() time.Time {
	if !p.LastMessage.IsZero() {
		return p.LastMessage.Time
	}
	if !p.DateServer.IsZero() {
		return p.DateServer.Time
	}
	return p.DateTracker.Time
}

// state derives the stored reporting state from a polled tracker record.
// Expiry is decided by the active flag alone: remaining_days legitimately hits
// zero on a renewal day, and calling a renewing subscription expired would
// blank the indicator for a working collar.
func (t weenectTrackerState) state(now time.Time) TrackerState {
	s := TrackerState{
		SubscriptionActive: t.Active,
		RemainingDays:      t.RemainingDays,
		CheckedAt:          now,
	}

	var pos *weenectStatePosition
	if len(t.Position) > 0 {
		pos = &t.Position[0]
	}
	if pos != nil {
		s.LastFixAt = pos.timestamp()
		if pos.OffReason != nil && *pos.OffReason != "" {
			s.OffReason = pos.OffReason
		}
	}

	switch {
	case !t.Active:
		s.Power = PowerExpired
	case pos == nil:
		// The tracker exists but has never reported a fix, so there is
		// nothing to base a claim about the device on.
		s.Power = PowerUnknown
	case pos.IsInDeepSleep:
		s.Power = PowerSleeping
	case pos.IsOnline:
		s.Power = PowerLive
	case s.OffReason != nil:
		s.Power = PowerOff
	default:
		s.Power = PowerOffline
	}

	return s
}
