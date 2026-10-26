package usagestore

import (
	"math"
	"sort"
)

// QuotaModel holds the statistical valuation fitted from all samples of one credential and period.
type QuotaModel struct {
	AuthIndex        string              `json:"auth_index"`
	Provider         string              `json:"provider"`
	Pool             string              `json:"pool"`
	PeriodSeconds    int64               `json:"period_seconds"`
	Status           string              `json:"status"` // "fitted" | "insufficient"
	Windows          int                 `json:"windows"`
	Segments         int                 `json:"segments"`
	ExternalSegments int                 `json:"external_segments"`
	CostUsed         float64             `json:"cost_used"`
	PctPerUSD        *float64            `json:"pct_per_usd"`
	FullCost         *float64            `json:"full_cost"`
	FullCostLow      *float64            `json:"full_cost_low"`
	FullCostHigh     *float64            `json:"full_cost_high"`
	CIPercent        *float64            `json:"ci_percent"`
	Features         []QuotaModelFeature `json:"features"`
	ModelValues      []QuotaModelValue   `json:"model_values"`
}

// QuotaModelFeature is one model and token kind feature and its fitted quota multiplier.
type QuotaModelFeature struct {
	Model      string  `json:"model"`
	Kind       string  `json:"kind"`
	Multiplier float64 `json:"multiplier"`
	Share      float64 `json:"share"`
}

// QuotaModelValue is the full-window value if all traffic were a single model with its observed token mix.
type QuotaModelValue struct {
	Model    string  `json:"model"`
	FullCost float64 `json:"full_cost"`
}

type segmentModelKind struct {
	model string
	kind  string // "input", "cache_read", "cache_write", "output"
}

type quotaSegment struct {
	windowIdx int
	deltaU    float64
	endMs     int64
	cost      float64
	unpriced  bool
	kindCosts map[segmentModelKind]float64
}

type fitResultLayer1 struct {
	status           string // "fitted" | "insufficient"
	k                float64
	fullCost         *float64
	fullCostLow      *float64
	fullCostHigh     *float64
	ciPercent        *float64
	costUsed         float64
	windowsCount     int
	segmentsCount    int
	externalSegments int
	isExternal       map[int]bool // segment index in usable -> bool
}

// fitLayer1 fits k = sum(w * deltaU) / sum(w * cost) using a 3-iteration robust loop.
func fitLayer1(segments []quotaSegment, weights []float64) fitResultLayer1 {
	usable := make([]int, 0, len(segments))
	for idx, s := range segments {
		if !s.unpriced && (s.cost > 0 || s.deltaU > 0) && s.deltaU >= 0 {
			usable = append(usable, idx)
		}
	}
	if len(usable) < 3 {
		return fitResultLayer1{status: "insufficient", isExternal: make(map[int]bool)}
	}

	isExternal := make(map[int]bool)
	var k float64

	for iter := 0; iter < 3; iter++ {
		sumWDeltaU := 0.0
		sumWCost := 0.0
		nonExtCount := 0
		for _, idx := range usable {
			if isExternal[idx] {
				continue
			}
			nonExtCount++
			w := weights[idx]
			sumWDeltaU += w * segments[idx].deltaU
			sumWCost += w * segments[idx].cost
		}
		if nonExtCount < 3 || sumWCost <= 0 {
			return fitResultLayer1{status: "insufficient", isExternal: make(map[int]bool)}
		}
		k = sumWDeltaU / sumWCost
		if k <= 0 {
			return fitResultLayer1{status: "insufficient", isExternal: make(map[int]bool)}
		}

		for _, idx := range usable {
			s := segments[idx]
			if s.deltaU >= 2 && k*s.cost < 0.2*s.deltaU {
				isExternal[idx] = true
			} else {
				isExternal[idx] = false
			}
		}
	}

	sumWDeltaU := 0.0
	sumWCost := 0.0
	nonExtCount := 0
	costUsed := 0.0
	extCount := 0
	for _, idx := range usable {
		if isExternal[idx] {
			extCount++
			continue
		}
		nonExtCount++
		w := weights[idx]
		sumWDeltaU += w * segments[idx].deltaU
		sumWCost += w * segments[idx].cost
		costUsed += segments[idx].cost
	}
	if nonExtCount < 3 || sumWCost <= 0 {
		return fitResultLayer1{status: "insufficient", isExternal: make(map[int]bool)}
	}
	k = sumWDeltaU / sumWCost
	if k <= 0 {
		return fitResultLayer1{status: "insufficient", isExternal: make(map[int]bool)}
	}
	fc := 100.0 / k

	// Confidence intervals across distinct windows
	type winAccum struct {
		sumU float64
		sumC float64
		sumW float64
		nSeg int
	}
	winMap := make(map[int]*winAccum)
	for _, idx := range usable {
		if isExternal[idx] {
			continue
		}
		wIdx := segments[idx].windowIdx
		wa := winMap[wIdx]
		if wa == nil {
			wa = &winAccum{}
			winMap[wIdx] = wa
		}
		wa.sumU += segments[idx].deltaU
		wa.sumC += segments[idx].cost
		wa.sumW += weights[idx]
		wa.nSeg++
	}

	type winRatio struct {
		k float64
		w float64
	}
	var winRatios []winRatio
	for _, wa := range winMap {
		if wa.sumC > 0 && wa.nSeg > 0 {
			avgW := wa.sumW / float64(wa.nSeg)
			kw := wa.sumU / wa.sumC
			winRatios = append(winRatios, winRatio{k: kw, w: wa.sumC * avgW})
		}
	}

	n := len(winRatios)
	var ciPercent, fcLow, fcHigh *float64
	if n >= 2 {
		sumW := 0.0
		for _, wr := range winRatios {
			sumW += wr.w
		}
		if sumW > 0 {
			sumSq := 0.0
			for _, wr := range winRatios {
				d := wr.k - k
				sumSq += wr.w * wr.w * d * d
			}
			seSquared := (float64(n) / float64(n-1)) * (sumSq / (sumW * sumW))
			if seSquared >= 0 {
				se := math.Sqrt(seSquared) / k
				if isFinite(se) {
					ci := 196.0 * se
					low := 100.0 / (k * (1.0 + 1.96*se))
					highDenom := 1.0 - 1.96*se
					if highDenom < 0.05 {
						highDenom = 0.05
					}
					high := 100.0 / (k * highDenom)
					ciPercent = &ci
					fcLow = &low
					fcHigh = &high
				}
			}
		}
	}

	return fitResultLayer1{
		status:           "fitted",
		k:                k,
		fullCost:         &fc,
		fullCostLow:      fcLow,
		fullCostHigh:     fcHigh,
		ciPercent:        ciPercent,
		costUsed:         costUsed,
		windowsCount:     n,
		segmentsCount:    nonExtCount,
		externalSegments: extCount,
		isExternal:       isExternal,
	}
}

// solveGaussian solves A * x = b using Gaussian elimination with partial pivoting.
func solveGaussian(A [][]float64, b []float64) ([]float64, bool) {
	n := len(b)
	if n == 0 {
		return []float64{}, true
	}
	if len(A) != n {
		return nil, false
	}
	M := make([][]float64, n)
	for i := 0; i < n; i++ {
		if len(A[i]) != n {
			return nil, false
		}
		M[i] = append(make([]float64, 0, n), A[i]...)
	}
	y := append(make([]float64, 0, n), b...)

	for col := 0; col < n; col++ {
		pivotRow := col
		maxVal := math.Abs(M[col][col])
		for row := col + 1; row < n; row++ {
			if v := math.Abs(M[row][col]); v > maxVal {
				maxVal = v
				pivotRow = row
			}
		}
		if maxVal < 1e-12 {
			return nil, false
		}
		if pivotRow != col {
			M[col], M[pivotRow] = M[pivotRow], M[col]
			y[col], y[pivotRow] = y[pivotRow], y[col]
		}
		pivot := M[col][col]
		for row := col + 1; row < n; row++ {
			factor := M[row][col] / pivot
			for c := col; c < n; c++ {
				M[row][c] -= factor * M[col][c]
			}
			y[row] -= factor * y[col]
		}
	}

	x := make([]float64, n)
	for row := n - 1; row >= 0; row-- {
		sum := y[row]
		for col := row + 1; col < n; col++ {
			sum -= M[row][col] * x[col]
		}
		x[row] = sum / M[row][row]
	}
	return x, true
}

// fitLayer2 fits per-(model, kind) multipliers using ridge regression.
func fitLayer2(segments []quotaSegment, weights []float64, k float64, isExternal map[int]bool) ([]QuotaModelFeature, []QuotaModelValue) {
	var usableNonExt []int
	for idx, s := range segments {
		if !s.unpriced && (s.cost > 0 || s.deltaU > 0) && s.deltaU >= 0 && !isExternal[idx] {
			usableNonExt = append(usableNonExt, idx)
		}
	}
	if len(usableNonExt) == 0 || k <= 0 {
		return []QuotaModelFeature{}, []QuotaModelValue{}
	}

	featTotalZ := make(map[segmentModelKind]float64)
	for _, idx := range usableNonExt {
		s := segments[idx]
		w := weights[idx]
		for mk, c := range s.kindCosts {
			if c > 0 {
				featTotalZ[mk] += w * k * c
			}
		}
	}
	totalZ := 0.0
	for _, z := range featTotalZ {
		totalZ += z
	}
	if totalZ <= 0 || len(featTotalZ) == 0 {
		return []QuotaModelFeature{}, []QuotaModelValue{}
	}

	type featSort struct {
		key segmentModelKind
		z   float64
	}
	var allFeats []featSort
	for mk, z := range featTotalZ {
		allFeats = append(allFeats, featSort{key: mk, z: z})
	}
	sort.Slice(allFeats, func(i, j int) bool {
		if allFeats[i].z != allFeats[j].z {
			return allFeats[i].z > allFeats[j].z
		}
		if allFeats[i].key.model != allFeats[j].key.model {
			return allFeats[i].key.model < allFeats[j].key.model
		}
		return allFeats[i].key.kind < allFeats[j].key.kind
	})

	var topFeats []segmentModelKind
	topSet := make(map[segmentModelKind]int)
	var hasOther bool
	for i, f := range allFeats {
		share := f.z / totalZ
		if i < 32 && share >= 0.02 {
			topSet[f.key] = len(topFeats)
			topFeats = append(topFeats, f.key)
		} else {
			hasOther = true
		}
	}

	P := len(topFeats)
	var otherIdx int
	if hasOther {
		otherIdx = P
		P++
	}

	// shortcut: fixed ridge strength in squared percentage points
	lambda := 4.0
	A := make([][]float64, P)
	for p := 0; p < P; p++ {
		A[p] = make([]float64, P)
		A[p][p] = lambda
	}
	b := make([]float64, P)
	for p := 0; p < P; p++ {
		b[p] = lambda // prior m_f = 1
	}

	for _, idx := range usableNonExt {
		s := segments[idx]
		w := weights[idx]
		du := s.deltaU
		z := make([]float64, P)
		for mk, c := range s.kindCosts {
			if c <= 0 {
				continue
			}
			val := k * c
			if pIdx, ok := topSet[mk]; ok {
				z[pIdx] += val
			} else if hasOther {
				z[otherIdx] += val
			}
		}
		for p := 0; p < P; p++ {
			b[p] += w * z[p] * du
			for q := 0; q < P; q++ {
				A[p][q] += w * z[p] * z[q]
			}
		}
	}

	m, ok := solveGaussian(A, b)
	if !ok {
		return []QuotaModelFeature{}, []QuotaModelValue{}
	}
	for p := 0; p < P; p++ {
		m[p] = math.Min(10, math.Max(0, m[p]))
	}

	var features []QuotaModelFeature
	for p, mk := range topFeats {
		share := (featTotalZ[mk] / totalZ) * 100.0
		features = append(features, QuotaModelFeature{
			Model:      mk.model,
			Kind:       mk.kind,
			Multiplier: m[p],
			Share:      share,
		})
	}
	if hasOther {
		otherZ := 0.0
		for mk, z := range featTotalZ {
			if _, ok := topSet[mk]; !ok {
				otherZ += z
			}
		}
		share := (otherZ / totalZ) * 100.0
		features = append(features, QuotaModelFeature{
			Model:      "",
			Kind:       "other",
			Multiplier: m[otherIdx],
			Share:      share,
		})
	}
	sort.Slice(features, func(i, j int) bool {
		if features[i].Share != features[j].Share {
			return features[i].Share > features[j].Share
		}
		if features[i].Model != features[j].Model {
			return features[i].Model < features[j].Model
		}
		return features[i].Kind < features[j].Kind
	})

	// Model values for models with share >= 2%
	modelZ := make(map[string]float64)
	for mk, z := range featTotalZ {
		modelZ[mk.model] += z
	}

	var modelValues []QuotaModelValue
	for model, mZ := range modelZ {
		if mZ/totalZ < 0.02 {
			continue
		}
		denomFactor := 0.0
		for _, kind := range []string{"input", "cache_read", "cache_write", "output"} {
			mk := segmentModelKind{model: model, kind: kind}
			zKind := featTotalZ[mk]
			if zKind <= 0 {
				continue
			}
			sKind := zKind / mZ
			var mult float64
			if pIdx, ok := topSet[mk]; ok {
				mult = m[pIdx]
			} else if hasOther {
				mult = m[otherIdx]
			} else {
				mult = 1.0
			}
			denomFactor += sKind * mult
		}
		if denomFactor > 0 && k > 0 {
			fullCost := 100.0 / (k * denomFactor)
			modelValues = append(modelValues, QuotaModelValue{
				Model:    model,
				FullCost: fullCost,
			})
		}
	}
	sort.Slice(modelValues, func(i, j int) bool {
		zi := modelZ[modelValues[i].Model]
		zj := modelZ[modelValues[j].Model]
		if zi != zj {
			return zi > zj
		}
		return modelValues[i].Model < modelValues[j].Model
	})

	if features == nil {
		features = []QuotaModelFeature{}
	}
	if modelValues == nil {
		modelValues = []QuotaModelValue{}
	}
	return features, modelValues
}
