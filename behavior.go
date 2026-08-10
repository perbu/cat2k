package main

import (
	"math"
	"sort"
	"sync"
	"time"
)

// HMM behavioral segmentation: rest / forage / transit (GitHub issue #5).
//
// Standard movement-ecology practice at our ~60s fix rate: classify every
// time window into a behavioral state with a 3-state hidden Markov model over
// step length (gamma emission) and turning angle (von Mises emission):
//
//   - rest    — short steps (GPS jitter around a point)
//   - forage  — moderate steps, high turning (exploring, hunting)
//   - transit — long steps, low turning (directed commute)
//
// Pipeline: gate fixes (FixGPS only — invalid fixes are noise and snapped
// CMD-X WiFi-zone fixes would fabricate a giant phantom rest bout at the zone
// center), resample to a regular 60s grid, derive steps, fit the HMM with
// Baum-Welch on a trailing training window, and decode request windows with
// Viterbi against the persisted parameters.
//
// Resampling: fixes are binned into behaviorSlotS-second slots (the fix
// nearest the slot center, within behaviorSlotTolS). Steps exist only between
// consecutive occupied slots, so a missed fix or a signal gap breaks the
// chain exactly the way activity.go refuses to measure across gaps. A step
// implying a speed above activityMaxSpeedMS is an impossible jump and breaks
// the chain the same way. No interpolation across gaps: the model never
// invents movement where there is no data.
//
// Turning angles are only computed when both adjacent steps clear
// behaviorTurnMinStepM: below that the heading is GPS jitter, not movement.
// Steps without a usable turn skip the angular emission term entirely (it
// would be uniform noise in every state), so rest is identified by step
// length alone — which is exactly what jitter around a nap spot looks like.
//
// Fix weighting: the HMM is a discrete-state decoder, so FixWeight has no
// natural place in it. Gating does the honest work (jitter reads as rest,
// not forage); the gamma emission's shape parameter absorbs the remaining
// per-fix accuracy differences.
//
// Model persistence: Baum-Welch is too expensive to run per request, so
// fitted parameters are stored as a JSON blob in the behavior_models table
// (one row per tracker). A model is refit on demand when it is missing or
// older than behaviorModelMaxAgeDays; every endpoint decodes against the
// stored parameters, so refits happen at most once per week per tracker no
// matter how often the UI polls. If a refit finds too little fresh training
// data, the stale model keeps serving; if there is no model at all, the
// endpoints report model_ready=false and empty payloads instead of
// fabricating states.
//
// State identity: EM does not know semantics, so after fitting the states are
// relabeled by mean step length (k*theta) ascending — index 0 rest, 1 forage,
// 2 transit. Never rely on the initial ordering surviving EM; relabel every
// time.

const (
	// behaviorSlotS: resampling interval. Matches the collar's tracking rate.
	behaviorSlotS = 60.0
	// behaviorSlotTolS: a fix must land within this of a slot center to fill
	// the slot (the collar's 60s cadence jitters by a few seconds).
	behaviorSlotTolS = 30.0
	// behaviorStepFloorM: gamma emissions are evaluated at max(step, floor);
	// a zero-length step would singularity the (k-1)*log(x) term at k>1.
	behaviorStepFloorM = 0.5
	// behaviorTurnMinStepM: both steps adjacent to a turn must clear this for
	// the turn angle to mean anything (below it, heading is jitter).
	behaviorTurnMinStepM = 5.0
	// behaviorTrainDays: trailing window a model is fit on.
	behaviorTrainDays = 60
	// behaviorMaxTrainSteps: cap on training steps (most recent win); keeps
	// EM interactive at multi-year data volumes.
	behaviorMaxTrainSteps = 30000
	// behaviorMinTrainSteps: below this a fit is noise; no model is stored.
	behaviorMinTrainSteps = 500
	// behaviorModelMaxAgeDays: stored params older than this are refit on the
	// next request.
	behaviorModelMaxAgeDays = 7
	// behaviorEMMaxIter / behaviorEMTol: Baum-Welch stops at the iteration
	// cap or when the relative log-likelihood gain falls below the tolerance.
	behaviorEMMaxIter = 30
	behaviorEMTol     = 1e-4
	// behaviorMaxSegSteps: /api/behavior/segments payload cap (mirrors the
	// 10000-row cap in GetPositions); the most recent steps are kept.
	behaviorMaxSegSteps = 10000
	// behaviorTransSmooth: Dirichlet-style pseudocount on transition counts;
	// keeps states sticky and zero-probability transitions impossible.
	behaviorTransSmooth = 0.01
)

// Behavioral state indices, valid AFTER relabeling (see relabelHMM).
const (
	StateRest = iota
	StateForage
	StateTransit
)

// behaviorStateNames maps a state index to its API/UI name.
var behaviorStateNames = [3]string{"rest", "forage", "transit"}

// behaviorStep is one resampled 60s step between consecutive occupied slots.
type behaviorStep struct {
	t                time.Time // timestamp of the step's end fix
	fromLat, fromLon float64
	toLat, toLon     float64
	stepM            float64
	turnRad          float64 // signed turn at the step, [-pi, pi]
	hasTurn          bool    // false when either adjacent step < behaviorTurnMinStepM
}

// hmmParams are the fitted 3-state HMM parameters. The JSON form is what
// behavior_models.params stores, so decoding never refits per request.
type hmmParams struct {
	K       [3]float64    `json:"k"`       // gamma shape per state
	Theta   [3]float64    `json:"theta"`   // gamma scale per state (m)
	Kappa   [3]float64    `json:"kappa"`   // von Mises concentration per state
	Trans   [3][3]float64 `json:"trans"`   // row-stochastic transition matrix
	Initial [3]float64    `json:"initial"` // initial state distribution
}

// meanStepM is the state's gamma mean step length — the semantic ordering key.
func (p *hmmParams) meanStepM(state int) float64 {
	return p.K[state] * p.Theta[state]
}

// logEmission is the log likelihood of a step under a state: gamma in the
// step length, von Mises (mu=0) in the turn when the turn is usable.
func (p *hmmParams) logEmission(s behaviorStep, state int) float64 {
	x := math.Max(s.stepM, behaviorStepFloorM)
	k, theta := p.K[state], p.Theta[state]
	lg, _ := math.Lgamma(k)
	ll := (k-1)*math.Log(x) - x/theta - k*math.Log(theta) - lg
	if s.hasTurn {
		kappa := p.Kappa[state]
		ll += kappa*math.Cos(s.turnRad) - math.Log(2*math.Pi) - math.Log(besselI0(kappa))
	}
	return ll
}

// emission is the exponentiated emission with an underflow floor for the
// probability-space forward-backward recursions.
func (p *hmmParams) emission(s behaviorStep, state int) float64 {
	return math.Max(math.Exp(p.logEmission(s, state)), 1e-300)
}

// besselI0 is the modified Bessel function of the first kind, order 0 —
// the von Mises normalizer. Numerical Recipes polynomial/rational
// approximation, accurate to ~1e-7.
func besselI0(x float64) float64 {
	ax := math.Abs(x)
	if ax < 3.75 {
		y := (x / 3.75) * (x / 3.75)
		return 1.0 + y*(3.5156229+y*(3.0899424+y*(1.2067492+
			y*(0.2659732+y*(0.0360768+y*0.0045813)))))
	}
	y := 3.75 / ax
	return math.Exp(ax) / math.Sqrt(ax) * (0.39894228 + y*(0.01328592+
		y*(0.00225319+y*(-0.00157565+y*(0.00916281+y*(-0.02057706+
			y*(0.02635537+y*(-0.01647633+y*0.00392377))))))))
}

// resampleBehaviorSteps gates fixes (FixGPS only), bins them to 60s slots
// (nearest fix to the slot center within behaviorSlotTolS), and derives steps
// between consecutive occupied slots, returned as segments: a missed slot or
// an impossible-speed jump breaks the segment so no step — and no turn — ever
// spans a gap. Fixes must be time-ordered (the range queries are).
func resampleBehaviorSteps(fixes []DwellPosition) [][]behaviorStep {
	type slotFix struct {
		t        time.Time
		lat, lon float64
	}
	slots := make(map[int64]slotFix)
	bestDelta := make(map[int64]float64)

	for _, p := range fixes {
		if ClassifyFix(p.ValidSignal, p.Type) != FixGPS {
			continue
		}
		unix := float64(p.Timestamp.Unix())
		slot := int64(math.Floor(unix / behaviorSlotS))
		delta := math.Abs(unix - (float64(slot)*behaviorSlotS + behaviorSlotS/2))
		if delta > behaviorSlotTolS {
			continue
		}
		if d, ok := bestDelta[slot]; !ok || delta < d {
			bestDelta[slot] = delta
			slots[slot] = slotFix{t: p.Timestamp, lat: p.Latitude, lon: p.Longitude}
		}
	}

	keys := make([]int64, 0, len(slots))
	for k := range slots {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return keys[a] < keys[b] })

	var segs [][]behaviorStep
	var cur []behaviorStep
	flush := func() {
		if len(cur) > 0 {
			segs = append(segs, cur)
			cur = nil
		}
	}

	for i := 0; i+1 < len(keys); i++ {
		if keys[i+1] != keys[i]+1 {
			flush() // signal gap: no step across missing slots
			continue
		}
		a, b := slots[keys[i]], slots[keys[i+1]]
		d := haversineDistance(a.lat, a.lon, b.lat, b.lon)
		if d/behaviorSlotS > activityMaxSpeedMS {
			flush() // impossible jump: bad fix, break the chain like a gap
			continue
		}
		cur = append(cur, behaviorStep{
			t: b.t, fromLat: a.lat, fromLon: a.lon,
			toLat: b.lat, toLon: b.lon, stepM: d,
		})
	}
	flush()

	// Turning angles, per segment (never across a break). Both adjacent steps
	// must clear the length floor for the heading to be meaningful.
	for _, seg := range segs {
		for i := 1; i < len(seg); i++ {
			if seg[i-1].stepM < behaviorTurnMinStepM || seg[i].stepM < behaviorTurnMinStepM {
				continue
			}
			b1 := calculateBearing(seg[i-1].fromLat, seg[i-1].fromLon, seg[i-1].toLat, seg[i-1].toLon)
			b2 := calculateBearing(seg[i].fromLat, seg[i].fromLon, seg[i].toLat, seg[i].toLon)
			turn := (b2 - b1) * math.Pi / 180
			seg[i].turnRad = math.Atan2(math.Sin(turn), math.Cos(turn))
			seg[i].hasTurn = true
		}
	}

	return segs
}

// countBehaviorSteps sums steps over segments.
func countBehaviorSteps(segs [][]behaviorStep) int {
	n := 0
	for _, s := range segs {
		n += len(s)
	}
	return n
}

// initHMMParams seeds the EM from step-length quantiles: the shortest third
// of steps seeds rest, the middle up to the 80th percentile seeds forage, the
// top fifth seeds transit. The semantic ordering only has to be close — EM
// moves the parameters and relabelHMM enforces the ordering afterwards.
func initHMMParams(steps []behaviorStep) hmmParams {
	lens := make([]float64, len(steps))
	for i, s := range steps {
		lens[i] = s.stepM
	}
	sort.Float64s(lens)
	q1 := lens[len(lens)/3]
	q2 := lens[4*len(lens)/5]

	// Moment-match a gamma per group, clamped to shapes the log-pdf survives.
	momentGamma := func(lo, hi float64) (k, theta float64) {
		var sum, sum2 float64
		n := 0
		for _, s := range steps {
			if s.stepM >= lo && (hi < 0 || s.stepM < hi) {
				sum += s.stepM
				sum2 += s.stepM * s.stepM
				n++
			}
		}
		if n == 0 {
			return 2.0, 10.0
		}
		mean := sum / float64(n)
		variance := math.Max(sum2/float64(n)-mean*mean, 0.25) // >= 0.5 m sigma
		k = math.Min(math.Max(mean*mean/variance, 1.01), 25)
		theta = math.Min(math.Max(variance/math.Max(mean, 0.1), 0.5), 500)
		return k, theta
	}

	var p hmmParams
	p.K[0], p.Theta[0] = momentGamma(-1, q1)
	p.K[1], p.Theta[1] = momentGamma(q1, q2)
	p.K[2], p.Theta[2] = momentGamma(q2, -1)
	// Angle priors: rest ~uniform, forage wandering, transit directed.
	p.Kappa = [3]float64{0.1, 1.0, 4.0}
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			if i == j {
				p.Trans[i][j] = 0.9
			} else {
				p.Trans[i][j] = 0.05
			}
		}
		p.Initial[i] = 1.0 / 3
	}
	return p
}

// fitHMM runs Baum-Welch over the step segments and returns relabeled
// parameters (state 0 = rest, 1 = forage, 2 = transit, by mean step length).
// Segments are treated as independent chains: the initial distribution
// applies at each segment head and no transition ever crosses a gap.
func fitHMM(segs [][]behaviorStep) hmmParams {
	var all []behaviorStep
	for _, s := range segs {
		all = append(all, s...)
	}
	p := initHMMParams(all)

	prevLL := math.Inf(-1)
	for iter := 0; iter < behaviorEMMaxIter; iter++ {
		var transCnt [3][3]float64
		var initCnt [3]float64
		var wSum, xSum, x2Sum, cosSum, sinSum [3]float64
		totalLL := 0.0

		for _, seg := range segs {
			T := len(seg)
			if T == 0 {
				continue
			}

			// Forward pass with per-step scaling (Rabiner). c[t] is the
			// scaling factor; the segment's log likelihood is sum(log c[t]).
			alpha := make([][3]float64, T)
			c := make([]float64, T)
			for i := 0; i < 3; i++ {
				alpha[0][i] = p.Initial[i] * p.emission(seg[0], i)
			}
			for t := 0; t < T; t++ {
				if t > 0 {
					for j := 0; j < 3; j++ {
						sum := 0.0
						for i := 0; i < 3; i++ {
							sum += alpha[t-1][i] * p.Trans[i][j]
						}
						alpha[t][j] = sum * p.emission(seg[t], j)
					}
				}
				for i := 0; i < 3; i++ {
					c[t] += alpha[t][i]
				}
				if c[t] < 1e-300 {
					c[t] = 1e-300
				}
				for i := 0; i < 3; i++ {
					alpha[t][i] /= c[t]
				}
				totalLL += math.Log(c[t])
			}

			// Backward pass (same scaling).
			beta := make([][3]float64, T)
			for i := 0; i < 3; i++ {
				beta[T-1][i] = 1
			}
			for t := T - 2; t >= 0; t-- {
				for i := 0; i < 3; i++ {
					sum := 0.0
					for j := 0; j < 3; j++ {
						sum += p.Trans[i][j] * p.emission(seg[t+1], j) * beta[t+1][j]
					}
					beta[t][i] = sum / c[t+1]
				}
			}

			// Accumulate sufficient statistics.
			for i := 0; i < 3; i++ {
				initCnt[i] += alpha[0][i] * beta[0][i]
			}
			for t := 0; t < T; t++ {
				for i := 0; i < 3; i++ {
					g := alpha[t][i] * beta[t][i]
					x := math.Max(seg[t].stepM, behaviorStepFloorM)
					wSum[i] += g
					xSum[i] += g * x
					x2Sum[i] += g * x * x
					if seg[t].hasTurn {
						cosSum[i] += g * math.Cos(seg[t].turnRad)
						sinSum[i] += g * math.Sin(seg[t].turnRad)
					}
				}
				if t+1 < T {
					for i := 0; i < 3; i++ {
						for j := 0; j < 3; j++ {
							transCnt[i][j] += alpha[t][i] * p.Trans[i][j] *
								p.emission(seg[t+1], j) * beta[t+1][j] / c[t+1]
						}
					}
				}
			}
		}

		// M-step.
		initTotal := 0.0
		for i := 0; i < 3; i++ {
			initTotal += initCnt[i]
		}
		for i := 0; i < 3; i++ {
			row := 0.0
			for j := 0; j < 3; j++ {
				row += transCnt[i][j] + behaviorTransSmooth
			}
			for j := 0; j < 3; j++ {
				p.Trans[i][j] = (transCnt[i][j] + behaviorTransSmooth) / row
			}
			if initTotal > 0 {
				p.Initial[i] = initCnt[i] / initTotal
			}
			w := wSum[i]
			if w < 1e-6 {
				continue // dead state: keep its parameters
			}
			mean := xSum[i] / w
			variance := math.Max(x2Sum[i]/w-mean*mean, 0.25)
			p.K[i] = math.Min(math.Max(mean*mean/variance, 1.01), 25)
			p.Theta[i] = math.Min(math.Max(variance/math.Max(mean, 0.1), 0.5), 500)
			// von Mises from the weighted mean resultant length (Banerjee's
			// approximation inverts A(kappa) without a Bessel ratio).
			r := math.Min(math.Hypot(cosSum[i], sinSum[i])/w, 0.999)
			p.Kappa[i] = math.Min(r*(2-r*r)/(1-r*r), 10)
		}

		if prevLL > math.Inf(-1) && math.Abs(totalLL-prevLL) < behaviorEMTol*math.Abs(totalLL) {
			break
		}
		prevLL = totalLL
	}

	return relabelHMM(p)
}

// relabelHMM permutes states so their gamma means sort ascending: index 0 is
// the shortest-step state (rest), 2 the longest (transit). EM can converge
// with any permutation of the initial ordering; semantics are attached here,
// never assumed from initialization.
func relabelHMM(p hmmParams) hmmParams {
	order := []int{0, 1, 2}
	sort.Slice(order, func(a, b int) bool {
		return p.meanStepM(order[a]) < p.meanStepM(order[b])
	})

	var out hmmParams
	for newIdx, oldIdx := range order {
		out.K[newIdx] = p.K[oldIdx]
		out.Theta[newIdx] = p.Theta[oldIdx]
		out.Kappa[newIdx] = p.Kappa[oldIdx]
		out.Initial[newIdx] = p.Initial[oldIdx]
		for newJ, oldJ := range order {
			out.Trans[newIdx][newJ] = p.Trans[oldIdx][oldJ]
		}
	}
	return out
}

// viterbiHMM decodes the most likely state sequence per segment, in log
// space. Returned slices parallel the input segments exactly.
func viterbiHMM(segs [][]behaviorStep, p *hmmParams) [][]int {
	const logZero = -1e30

	safeLog := func(v float64) float64 {
		if v <= 0 {
			return logZero
		}
		return math.Log(v)
	}

	out := make([][]int, len(segs))
	for si, seg := range segs {
		T := len(seg)
		states := make([]int, T)
		if T == 0 {
			out[si] = states
			continue
		}

		delta := make([][3]float64, T)
		psi := make([][3]int, T)
		for i := 0; i < 3; i++ {
			delta[0][i] = safeLog(p.Initial[i]) + p.logEmission(seg[0], i)
		}
		for t := 1; t < T; t++ {
			for j := 0; j < 3; j++ {
				best, arg := math.Inf(-1), 0
				for i := 0; i < 3; i++ {
					v := delta[t-1][i] + safeLog(p.Trans[i][j])
					if v > best {
						best, arg = v, i
					}
				}
				delta[t][j] = best + p.logEmission(seg[t], j)
				psi[t][j] = arg
			}
		}

		best, arg := math.Inf(-1), 0
		for i := 0; i < 3; i++ {
			if delta[T-1][i] > best {
				best, arg = delta[T-1][i], i
			}
		}
		states[T-1] = arg
		for t := T - 1; t > 0; t-- {
			states[t-1] = psi[t][states[t]]
		}
		out[si] = states
	}
	return out
}

// BehaviorDayBudget is one calendar day's decoded time per state.
type BehaviorDayBudget struct {
	Date     string  `json:"date"` // YYYY-MM-DD (local)
	RestS    float64 `json:"rest_s"`
	ForageS  float64 `json:"forage_s"`
	TransitS float64 `json:"transit_s"`
}

// behaviorBudgetPerDay buckets decoded steps into calendar days. Every step
// counts behaviorSlotS seconds of its state, attributed to the day of the
// step's end timestamp. Days without steps come back zeroed.
func behaviorBudgetPerDay(segs [][]behaviorStep, decoded [][]int, dates []string, loc *time.Location) []BehaviorDayBudget {
	idx := make(map[string]int, len(dates))
	out := make([]BehaviorDayBudget, len(dates))
	for i, d := range dates {
		out[i].Date = d
		idx[d] = i
	}

	for si, seg := range segs {
		for t, s := range seg {
			day, ok := idx[s.t.In(loc).Format("2006-01-02")]
			if !ok {
				continue
			}
			switch decoded[si][t] {
			case StateRest:
				out[day].RestS += behaviorSlotS
			case StateForage:
				out[day].ForageS += behaviorSlotS
			case StateTransit:
				out[day].TransitS += behaviorSlotS
			}
		}
	}
	return out
}

// behaviorHourMatrix aggregates decoded steps into a 24h x 3 matrix of state
// shares: each row is the share of decoded step time spent in each state
// during that local hour of day. Rows with no steps are all zero.
func behaviorHourMatrix(segs [][]behaviorStep, decoded [][]int, loc *time.Location) [24][3]float64 {
	var counts [24][3]float64
	for si, seg := range segs {
		for t, s := range seg {
			counts[s.t.In(loc).Hour()][decoded[si][t]] += behaviorSlotS
		}
	}

	var out [24][3]float64
	for h := 0; h < 24; h++ {
		total := counts[h][0] + counts[h][1] + counts[h][2]
		if total <= 0 {
			continue
		}
		for s := 0; s < 3; s++ {
			out[h][s] = counts[h][s] / total
		}
	}
	return out
}

// behaviorFitMu serializes model refits so concurrent first requests don't
// all run Baum-Welch at once.
var behaviorFitMu sync.Mutex

// behaviorModel returns the tracker's stored HMM parameters, refitting when
// the stored model is missing or older than behaviorModelMaxAgeDays. Returns
// (nil, nil) when there is too little training data and no stored model —
// callers treat that as "model not ready" and answer with empty payloads.
func (d *Database) behaviorModel(trackerID int) (*hmmParams, error) {
	stored, err := d.GetBehaviorModel(trackerID)
	if err != nil {
		return nil, err
	}
	if stored != nil && time.Since(stored.FittedAt) < behaviorModelMaxAgeDays*24*time.Hour {
		return &stored.Params, nil
	}

	behaviorFitMu.Lock()
	defer behaviorFitMu.Unlock()

	// Another request may have refit while we waited for the lock.
	stored, err = d.GetBehaviorModel(trackerID)
	if err != nil {
		return nil, err
	}
	if stored != nil && time.Since(stored.FittedAt) < behaviorModelMaxAgeDays*24*time.Hour {
		return &stored.Params, nil
	}

	since := time.Now().AddDate(0, 0, -behaviorTrainDays)
	fixes, err := d.GetPositionsForRange(trackerID, since, time.Time{})
	if err != nil {
		return nil, err
	}
	segs := resampleBehaviorSteps(fixes)

	// Cap training volume deterministically: the most recent steps are the
	// most representative of current collar behavior anyway.
	for countBehaviorSteps(segs) > behaviorMaxTrainSteps && len(segs) > 1 {
		segs = segs[1:]
	}

	if n := countBehaviorSteps(segs); n < behaviorMinTrainSteps {
		if stored != nil {
			// Too little fresh data to refit: keep serving the stale model.
			return &stored.Params, nil
		}
		return nil, nil
	} else {
		params := fitHMM(segs)
		if err := d.SaveBehaviorModel(trackerID, n, &params); err != nil {
			// Persistence failure is not fatal: decode with the fresh params.
			return &params, nil
		}
		return &params, nil
	}
}

// decodeBehaviorWindow resamples the fixes in [from, to) and Viterbi-decodes
// them against the given parameters.
func (d *Database) decodeBehaviorWindow(trackerID int, from, to time.Time, p *hmmParams) ([][]behaviorStep, [][]int, error) {
	fixes, err := d.GetPositionsForRange(trackerID, from, to)
	if err != nil {
		return nil, nil, err
	}
	segs := resampleBehaviorSteps(fixes)
	return segs, viterbiHMM(segs, p), nil
}
