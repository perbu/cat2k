package main

// Shared fix-quality gate for all analytics (GitHub issue #1).
//
// The Weenect API mixes genuine GPS fixes with WiFi-zone fixes whose
// coordinates are snapped to the zone center. Feeding snapped fixes into
// movement metrics fabricates stillness at the zone location, so every
// analysis must classify fixes through this gate before trusting coordinates.
//
// What the trackers actually report (from backfilled production data, 13k
// rows, 2 trackers): the real GPS fixes are typed CMD-T, CMD-F, ALM-V,
// ALM-BAT and ALM-GEO-I (satellites populated, speed set, varied accuracy
// radius). WiFi-zone presence is NOT marked by type — every in-zone fix
// carries wifi_zone_id != 0 and is snapped to a single coordinate pair (the
// zone center): CMD-WBT-OUT and CMD-WBT (zero satellites, no speed, flat
// radius) plus in-zone ALM-ON. The originally assumed zone-fix type CMD-X
// never occurs in practice; it is kept as harmless future-proofing.
// CMD-WIFIP fixes are WiFi-positioned: zero satellites, no speed, sticky
// coordinates, wifi_zone_id == 0 — neither GPS nor home-zone evidence, so
// the gate treats them as invalid for movement analytics.

// FixClass is the gate's verdict for a single position fix.
type FixClass int

const (
	// FixInvalid marks fixes to exclude from all analytics: no valid signal,
	// or WiFi-positioned fixes (type "CMD-WIFIP") that are neither GPS nor
	// home-zone evidence.
	FixInvalid FixClass = iota
	// FixHome marks WiFi-zone fixes (wifi_zone_id != 0, or the never-seen
	// type "CMD-X"): the tracker is inside a configured WiFi zone,
	// coordinates are snapped to the zone center. Usable as "at home"
	// presence evidence only, never for movement metrics.
	FixHome
	// FixGPS marks genuine GPS fixes usable for movement analysis, weighted
	// by their accuracy radius (see FixWeight).
	FixGPS
)

// wifiZoneFixType is a position type that, if it ever appears, would mark a
// WiFi-zone fix. It never occurs in observed data — wifi_zone_id != 0 is the
// real marker — but keeping it costs nothing.
const wifiZoneFixType = "CMD-X"

// wifiPositionFixType marks WiFi-positioned fixes: sticky coordinates with
// zero satellites and no home-zone membership. Not GPS, not presence
// evidence — excluded from all analytics.
const wifiPositionFixType = "CMD-WIFIP"

// ClassifyFix applies the shared quality gate to a fix. A nil wifiZoneID
// (pre-migration legacy row) is "not home", never FixHome.
func ClassifyFix(validSignal *bool, typ *string, wifiZoneID *int) FixClass {
	if validSignal != nil && !*validSignal {
		return FixInvalid
	}
	if wifiZoneID != nil && *wifiZoneID != 0 {
		return FixHome
	}
	if typ != nil {
		switch *typ {
		case wifiZoneFixType:
			return FixHome
		case wifiPositionFixType:
			return FixInvalid
		}
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
