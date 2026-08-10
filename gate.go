package main

// Shared fix-quality gate for all analytics (GitHub issue #1).
//
// The Weenect API mixes genuine GPS fixes with WiFi-zone fixes whose
// coordinates are snapped to the zone center. Feeding snapped fixes into
// movement metrics fabricates stillness at the zone location, so every
// analysis must classify fixes through this gate before trusting coordinates.

// FixClass is the gate's verdict for a single position fix.
type FixClass int

const (
	// FixInvalid marks fixes to exclude from all analytics (no valid signal).
	FixInvalid FixClass = iota
	// FixHome marks WiFi-zone fixes (type "CMD-X"): the tracker is inside a
	// configured WiFi zone, coordinates are snapped to the zone center. Usable
	// as "at home" presence evidence only, never for movement metrics.
	FixHome
	// FixGPS marks genuine GPS fixes usable for movement analysis, weighted
	// by their accuracy radius (see FixWeight).
	FixGPS
)

// wifiZoneFixType is the position type the collar reports for WiFi-zone fixes.
const wifiZoneFixType = "CMD-X"

// ClassifyFix applies the shared quality gate to a fix.
func ClassifyFix(validSignal *bool, typ *string) FixClass {
	if validSignal != nil && !*validSignal {
		return FixInvalid
	}
	if typ != nil && *typ == wifiZoneFixType {
		return FixHome
	}
	return FixGPS
}

// FixWeight returns the relative weight of a GPS fix for density-style
// analytics (KDE, clustering), inversely proportional to its accuracy radius.
// A fix with unknown radius gets weight 1; a crisp 5 m fix outweighs a sloppy
// 100 m fix twenty to one.
func FixWeight(radius *int) float64 {
	if radius == nil || *radius <= 0 {
		return 1.0
	}
	return 1.0 / float64(*radius)
}
