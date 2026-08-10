package main

import (
	"math"
	"sort"
	"time"
)

// KDE utilization distributions: 50% core range vs 95% home range contours
// (GitHub issue #3).
//
// Pipeline: gate fixes (only genuine GPS fixes — snapped WiFi-zone
// fixes would pile phantom mass at the zone center), weight each fix by
// FixWeight(radius), project to a local equirectangular plane (meters) around
// the data centroid, and evaluate a weighted Gaussian KDE on a regular grid.
// Sorting grid cells by density descending gives the density thresholds where
// cumulative mass reaches 50% / 95%; marching squares extracts the isopleth
// contours at those thresholds as polygons, lightly simplified with
// Douglas-Peucker and emitted as GeoJSON-style [lon,lat] rings.
//
// Bandwidth: 2x the median fix accuracy radius, clamped to
// [kdeMinBandwidthM, kdeMaxBandwidthM]. The median radius is the tracker's own
// estimate of its positional noise, so doubling it bridges GPS jitter between
// adjacent fixes without smearing real territory structure; the clamp keeps a
// month of sloppy 100 m fixes from producing a single blob.
//
// Performance: evaluation is O(fixes x stencil) where the stencil only covers
// cells within 3 bandwidths of a fix (the Gaussian tail beyond that is < 1.2%
// of peak). Fix counts are capped at kdeMaxFixes by deterministic stride
// subsampling (time-ordered, every k-th fix) so a dense month stays
// interactive; the cap is far above typical monthly volumes.

const (
	// kdeGridSize: cells per axis of the KDE grid.
	kdeGridSize = 128
	// kdeMaxFixes: deterministic subsample cap per KDE evaluation.
	kdeMaxFixes = 20000
	// kdeMinFixes: below this a monthly estimate is noise; contours are
	// omitted (and the overlap index reports null).
	kdeMinFixes = 30
	// kdeGridPadBandwidths: the grid extends this many bandwidths past the
	// fix bounding box, so contours always close inside the grid.
	kdeGridPadBandwidths = 3.0
	// kdeBandwidthFactor: bandwidth = factor x median fix radius.
	kdeBandwidthFactor = 2.0
	kdeMinBandwidthM   = 10.0
	kdeMaxBandwidthM   = 50.0
	// kdeDefaultRadiusM: assumed accuracy radius when no fix reports one.
	kdeDefaultRadiusM = 25.0
	// kdeStencilBandwidths: kernel cutoff radius, in bandwidths.
	kdeStencilBandwidths = 3.0
)

// localProjection is an equirectangular projection in meters around a
// reference point. Distortion is negligible over the few kilometers a cat
// covers.
type localProjection struct {
	lat0       float64 // reference latitude, degrees
	lon0       float64 // reference longitude, degrees
	mPerDegLat float64
	mPerDegLon float64
}

func newLocalProjection(lat0, lon0 float64) localProjection {
	const mPerDegLat = 111320.0 // close enough at these extents
	return localProjection{
		lat0:       lat0,
		lon0:       lon0,
		mPerDegLat: mPerDegLat,
		mPerDegLon: mPerDegLat * math.Cos(lat0*math.Pi/180),
	}
}

func (p localProjection) toXY(lat, lon float64) (x, y float64) {
	return (lon - p.lon0) * p.mPerDegLon, (lat - p.lat0) * p.mPerDegLat
}

func (p localProjection) toLatLon(x, y float64) (lat, lon float64) {
	return p.lat0 + y/p.mPerDegLat, p.lon0 + x/p.mPerDegLon
}

// kdeFix is a gated GPS fix in projected meters with its quality weight.
type kdeFix struct {
	x, y, w float64
}

// centroidProjection returns the equirectangular projection around the mean
// of the gated fixes.
func centroidProjection(positions []DwellPosition) (localProjection, int) {
	var sumLat, sumLon float64
	n := 0
	for _, p := range positions {
		if ClassifyFix(p.ValidSignal, p.Type, p.WifiZoneID) != FixGPS {
			continue
		}
		sumLat += p.Latitude
		sumLon += p.Longitude
		n++
	}
	if n == 0 {
		return localProjection{}, 0
	}
	return newLocalProjection(sumLat/float64(n), sumLon/float64(n)), n
}

// gateKDEFixes gates raw positions (FixGPS only), projects them with proj,
// weights them by FixWeight, and subsamples with a deterministic time-ordered
// stride when above kdeMaxFixes. Positions must be time-ordered (the
// gate-eligible range queries are).
func gateKDEFixes(positions []DwellPosition, proj localProjection, gatedCount int) []kdeFix {
	if gatedCount == 0 {
		return nil
	}

	stride := 1
	if gatedCount > kdeMaxFixes {
		stride = int(math.Ceil(float64(gatedCount) / float64(kdeMaxFixes)))
	}

	var fixes []kdeFix
	seen := 0
	for _, p := range positions {
		if ClassifyFix(p.ValidSignal, p.Type, p.WifiZoneID) != FixGPS {
			continue
		}
		if seen%stride != 0 {
			seen++
			continue
		}
		seen++
		x, y := proj.toXY(p.Latitude, p.Longitude)
		fixes = append(fixes, kdeFix{x: x, y: y, w: FixWeight(p.Radius)})
	}
	return fixes
}

// prepareKDEFixes gates and projects positions around their own centroid.
func prepareKDEFixes(positions []DwellPosition) ([]kdeFix, localProjection) {
	proj, n := centroidProjection(positions)
	return gateKDEFixes(positions, proj, n), proj
}

// kdeBandwidth picks the Gaussian bandwidth: kdeBandwidthFactor times the
// median reported fix radius, clamped to a sane range.
func kdeBandwidth(positions []DwellPosition) float64 {
	var radii []float64
	for _, p := range positions {
		if ClassifyFix(p.ValidSignal, p.Type, p.WifiZoneID) != FixGPS {
			continue
		}
		if p.Radius != nil && *p.Radius > 0 {
			radii = append(radii, float64(*p.Radius))
		}
	}

	median := kdeDefaultRadiusM
	if len(radii) > 0 {
		sort.Float64s(radii)
		median = radii[len(radii)/2]
	}

	h := kdeBandwidthFactor * median
	return math.Min(kdeMaxBandwidthM, math.Max(kdeMinBandwidthM, h))
}

// kdeGrid is a regular meter grid holding weighted KDE density values.
type kdeGrid struct {
	proj         localProjection
	nx, ny       int
	minX, minY   float64
	cellW, cellH float64
	density      []float64 // row-major, ny rows of nx cells
}

// newKDEGrid lays out a kdeGridSize x kdeGridSize grid over the fixes'
// bounding box padded by padM meters on every side.
func newKDEGrid(proj localProjection, fixes []kdeFix, padM float64) *kdeGrid {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, f := range fixes {
		minX = math.Min(minX, f.x)
		minY = math.Min(minY, f.y)
		maxX = math.Max(maxX, f.x)
		maxY = math.Max(maxY, f.y)
	}
	minX -= padM
	minY -= padM
	maxX += padM
	maxY += padM

	return &kdeGrid{
		proj:    proj,
		nx:      kdeGridSize,
		ny:      kdeGridSize,
		minX:    minX,
		minY:    minY,
		cellW:   (maxX - minX) / float64(kdeGridSize-1),
		cellH:   (maxY - minY) / float64(kdeGridSize-1),
		density: make([]float64, kdeGridSize*kdeGridSize),
	}
}

// evaluate computes the weighted Gaussian KDE with bandwidth h at every grid
// cell. Each fix only visits cells within kdeStencilBandwidths of itself; the
// kernel tail beyond that is negligible. The normalizing constant is omitted
// — consumers work with mass fractions, which are scale-invariant.
func (g *kdeGrid) evaluate(fixes []kdeFix, h float64) {
	cutoff := kdeStencilBandwidths * h
	invTwoH2 := 1.0 / (2.0 * h * h)

	for _, f := range fixes {
		i0 := int(math.Max(0, math.Floor((f.x-cutoff-g.minX)/g.cellW)))
		i1 := int(math.Min(float64(g.nx-1), math.Ceil((f.x+cutoff-g.minX)/g.cellW)))
		j0 := int(math.Max(0, math.Floor((f.y-cutoff-g.minY)/g.cellH)))
		j1 := int(math.Min(float64(g.ny-1), math.Ceil((f.y+cutoff-g.minY)/g.cellH)))

		for j := j0; j <= j1; j++ {
			cy := g.minY + float64(j)*g.cellH
			dy := cy - f.y
			for i := i0; i <= i1; i++ {
				cx := g.minX + float64(i)*g.cellW
				dx := cx - f.x
				g.density[j*g.nx+i] += f.w * math.Exp(-(dx*dx+dy*dy)*invTwoH2)
			}
		}
	}
}

// isoplethThreshold returns the density threshold above which the given
// fraction of the total weighted mass lies: sort cells by density descending
// and walk the cumulative mass until it crosses the fraction. Because every
// cell has equal area, mass fractions come straight from the density sums.
func (g *kdeGrid) isoplethThreshold(fraction float64) float64 {
	sorted := make([]float64, len(g.density))
	copy(sorted, g.density)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))

	total := 0.0
	for _, v := range sorted {
		total += v
	}
	if total <= 0 {
		return 0
	}

	cum := 0.0
	target := fraction * total
	for _, v := range sorted {
		cum += v
		if cum >= target {
			return v
		}
	}
	return sorted[len(sorted)-1]
}

// massAboveThreshold reports the fraction of total mass in cells at or above
// the threshold (the bookkeeping counterpart of isoplethThreshold).
func (g *kdeGrid) massAboveThreshold(threshold float64) float64 {
	total, above := 0.0, 0.0
	for _, v := range g.density {
		total += v
		if v >= threshold {
			above += v
		}
	}
	if total <= 0 {
		return 0
	}
	return above / total
}

// msPoint is a contour vertex in projected meters.
type msPoint struct {
	x, y float64
}

// msPointKey quantizes a vertex so the same grid edge evaluated from two
// neighboring quads chains together despite float rounding.
func msPointKey(p msPoint) [2]int64 {
	return [2]int64{int64(math.Round(p.x * 1000)), int64(math.Round(p.y * 1000))}
}

// msSegment is one contour piece across a grid quad.
type msSegment struct {
	a, b msPoint
}

// contourRings extracts the closed contour rings at a density threshold with
// marching squares, simplifies them with Douglas-Peucker, drops slivers, and
// returns GeoJSON-style rings of [lon, lat] pairs (explicitly closed).
func (g *kdeGrid) contourRings(threshold float64) [][][2]float64 {
	segments := g.marchingSquares(threshold)
	if len(segments) == 0 {
		return nil
	}

	// Chain segments into rings by shared endpoints. The grid is padded by
	// several bandwidths, so border density is ~0 and every contour closes.
	adjacency := make(map[[2]int64][]int, len(segments)*2)
	for i, s := range segments {
		adjacency[msPointKey(s.a)] = append(adjacency[msPointKey(s.a)], i)
		adjacency[msPointKey(s.b)] = append(adjacency[msPointKey(s.b)], i)
	}

	used := make([]bool, len(segments))
	var rings [][]msPoint
	for i, s := range segments {
		if used[i] {
			continue
		}
		used[i] = true
		ring := []msPoint{s.a, s.b}
		start := msPointKey(s.a)
		cur := msPointKey(s.b)

		for cur != start {
			next := -1
			for _, cand := range adjacency[cur] {
				if !used[cand] {
					next = cand
					break
				}
			}
			if next < 0 {
				break // numeric dead end; keep the partial ring
			}
			used[next] = true
			seg := segments[next]
			if msPointKey(seg.a) == cur {
				ring = append(ring, seg.b)
				cur = msPointKey(seg.b)
			} else {
				ring = append(ring, seg.a)
				cur = msPointKey(seg.a)
			}
		}
		rings = append(rings, ring)
	}

	// Simplify in meters, drop slivers, convert to [lon,lat].
	epsilon := 0.4 * math.Min(g.cellW, g.cellH)
	minArea := 2.0 * g.cellW * g.cellH
	var out [][][2]float64
	for _, ring := range rings {
		simple := douglasPeucker(ring, epsilon)
		if len(simple) < 3 || math.Abs(ringArea(simple)) < minArea {
			continue
		}
		ll := make([][2]float64, 0, len(simple)+1)
		for _, p := range simple {
			lat, lon := g.proj.toLatLon(p.x, p.y)
			ll = append(ll, [2]float64{lon, lat})
		}
		ll = append(ll, ll[0]) // close the ring explicitly
		out = append(out, ll)
	}
	return out
}

// marchingSquares emits the contour segments at a threshold over all grid
// quads. Corner order per quad: c00, c10, c11, c01; edge crossings are
// linearly interpolated along the bottom, right, top, and left edges.
func (g *kdeGrid) marchingSquares(threshold float64) []msSegment {
	var segments []msSegment

	at := func(i, j int) float64 { return g.density[j*g.nx+i] }
	pos := func(i, j int) msPoint {
		return msPoint{g.minX + float64(i)*g.cellW, g.minY + float64(j)*g.cellH}
	}
	// Crossing point on the edge between grid nodes p0 (value v0) and p1 (v1).
	interp := func(p0, p1 msPoint, v0, v1 float64) msPoint {
		t := (threshold - v0) / (v1 - v0)
		return msPoint{p0.x + t*(p1.x-p0.x), p0.y + t*(p1.y-p0.y)}
	}

	for j := 0; j+1 < g.ny; j++ {
		for i := 0; i+1 < g.nx; i++ {
			v00, v10 := at(i, j), at(i+1, j)
			v11, v01 := at(i+1, j+1), at(i, j+1)

			caseIdx := 0
			if v00 >= threshold {
				caseIdx |= 1
			}
			if v10 >= threshold {
				caseIdx |= 2
			}
			if v11 >= threshold {
				caseIdx |= 4
			}
			if v01 >= threshold {
				caseIdx |= 8
			}
			if caseIdx == 0 || caseIdx == 15 {
				continue
			}

			c00, c10 := pos(i, j), pos(i+1, j)
			c11, c01 := pos(i+1, j+1), pos(i, j+1)
			bottom := func() msPoint { return interp(c00, c10, v00, v10) }
			right := func() msPoint { return interp(c10, c11, v10, v11) }
			top := func() msPoint { return interp(c01, c11, v01, v11) }
			left := func() msPoint { return interp(c00, c01, v00, v01) }

			switch caseIdx {
			case 1, 14:
				segments = append(segments, msSegment{bottom(), left()})
			case 2, 13:
				segments = append(segments, msSegment{bottom(), right()})
			case 3, 12:
				segments = append(segments, msSegment{right(), left()})
			case 4, 11:
				segments = append(segments, msSegment{right(), top()})
			case 5: // saddle: c00, c11 inside
				segments = append(segments, msSegment{bottom(), left()}, msSegment{right(), top()})
			case 6, 9:
				segments = append(segments, msSegment{bottom(), top()})
			case 7, 8:
				segments = append(segments, msSegment{top(), left()})
			case 10: // saddle: c10, c01 inside
				segments = append(segments, msSegment{bottom(), right()}, msSegment{top(), left()})
			}
		}
	}
	return segments
}

// douglasPeucker simplifies a polyline (in meters) to the given tolerance.
func douglasPeucker(pts []msPoint, epsilon float64) []msPoint {
	if len(pts) < 3 {
		return pts
	}

	a, b := pts[0], pts[len(pts)-1]
	dx, dy := b.x-a.x, b.y-a.y
	denom := math.Hypot(dx, dy)

	maxDist := -1.0
	maxIdx := 0
	for i := 1; i < len(pts)-1; i++ {
		var dist float64
		if denom == 0 {
			dist = math.Hypot(pts[i].x-a.x, pts[i].y-a.y)
		} else {
			dist = math.Abs(dy*pts[i].x-dx*pts[i].y+b.x*a.y-b.y*a.x) / denom
		}
		if dist > maxDist {
			maxDist = dist
			maxIdx = i
		}
	}

	if maxDist <= epsilon {
		return []msPoint{a, b}
	}

	left := douglasPeucker(pts[:maxIdx+1], epsilon)
	right := douglasPeucker(pts[maxIdx:], epsilon)
	return append(left[:len(left)-1], right...)
}

// ringArea is the signed shoelace area of a meter-space ring.
func ringArea(ring []msPoint) float64 {
	area := 0.0
	for i := 0; i < len(ring); i++ {
		j := (i + 1) % len(ring)
		area += ring[i].x*ring[j].y - ring[j].x*ring[i].y
	}
	return area / 2
}

// bhattacharyya computes the Bhattacharyya affinity between two normalized
// mass distributions on the same grid: BC = sum(sqrt(m1_i * m2_i)) over
// cells, where m is per-cell probability mass (sums to 1). BC is 1 for
// identical distributions and 0 for disjoint ones. (The continuous form
// integral(sqrt(p1*p2) dA) reduces to exactly this on a shared grid once the
// densities are discretized to per-cell masses; cell area cancels.)
func bhattacharyya(g1, g2 *kdeGrid) float64 {
	total1, total2 := 0.0, 0.0
	for _, v := range g1.density {
		total1 += v
	}
	for _, v := range g2.density {
		total2 += v
	}
	if total1 <= 0 || total2 <= 0 {
		return 0
	}

	bc := 0.0
	for i := range g1.density {
		bc += math.Sqrt(g1.density[i] * g2.density[i])
	}
	return bc / math.Sqrt(total1*total2)
}

// rangeEstimate is the KDE result for one tracker over a period.
type rangeEstimate struct {
	BandwidthM float64        `json:"bandwidth_m"`
	FixesUsed  int            `json:"fixes_used"`
	Core       [][][2]float64 `json:"core"` // 50% isopleth rings, [lon,lat]
	Home       [][][2]float64 `json:"home"` // 95% isopleth rings, [lon,lat]
}

// computeRange runs the KDE pipeline for one tracker over [from, to): fetch
// gate-eligible positions, build the weighted KDE, and extract the 50% core
// and 95% home-range contours. Periods with fewer than kdeMinFixes gated
// fixes return an estimate with empty contours.
func (d *Database) computeRange(trackerID int, from, to time.Time) (*rangeEstimate, error) {
	positions, err := d.GetPositionsForRange(trackerID, from, to)
	if err != nil {
		return nil, err
	}

	fixes, proj := prepareKDEFixes(positions)
	est := &rangeEstimate{
		FixesUsed: len(fixes),
		Core:      [][][2]float64{},
		Home:      [][][2]float64{},
	}
	if len(fixes) < kdeMinFixes {
		return est, nil
	}

	h := kdeBandwidth(positions)
	est.BandwidthM = h

	grid := newKDEGrid(proj, fixes, kdeGridPadBandwidths*h)
	grid.evaluate(fixes, h)

	est.Core = nonNilRings(grid.contourRings(grid.isoplethThreshold(0.50)))
	est.Home = nonNilRings(grid.contourRings(grid.isoplethThreshold(0.95)))
	return est, nil
}

func nonNilRings(rings [][][2]float64) [][][2]float64 {
	if rings == nil {
		return [][][2]float64{}
	}
	return rings
}

// overlapIndex evaluates both cats' KDEs on a shared grid (combined bounding
// box around the joint centroid, padded by the larger bandwidth) and returns
// the Bhattacharyya affinity. Each cat keeps its own bandwidth.
func overlapIndex(posA, posB []DwellPosition) float64 {
	proj, nA := centroidProjection(posA)
	_, nB := centroidProjection(posB)
	if nA == 0 || nB == 0 {
		return 0
	}
	// Rebase the projection on the combined centroid so both KDEs share a
	// frame (and a grid).
	proj, _ = centroidProjection(append(posA, posB...))

	fixesA := gateKDEFixes(posA, proj, nA)
	fixesB := gateKDEFixes(posB, proj, nB)
	hA, hB := kdeBandwidth(posA), kdeBandwidth(posB)

	grid := newKDEGrid(proj, append(fixesA, fixesB...), kdeGridPadBandwidths*math.Max(hA, hB))

	gA := &kdeGrid{nx: grid.nx, ny: grid.ny, minX: grid.minX, minY: grid.minY, cellW: grid.cellW, cellH: grid.cellH, density: make([]float64, len(grid.density))}
	gB := &kdeGrid{nx: grid.nx, ny: grid.ny, minX: grid.minX, minY: grid.minY, cellW: grid.cellW, cellH: grid.cellH, density: make([]float64, len(grid.density))}
	gA.evaluate(fixesA, hA)
	gB.evaluate(fixesB, hB)

	return bhattacharyya(gA, gB)
}
