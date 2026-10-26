// Package sync aligns Dream Cast audio with a video reference track using
// normalized cross-correlation of side-channel (L-R) envelopes.
package sync

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"dreamtui/internal/audio"
)

// RefWindow is a reference audio window decoded from the original track.
type RefWindow struct {
	VideoTime float64
	Envelope  *audio.Envelope
}

// Anchor is one alignment measurement.
type Anchor struct {
	VideoTime float64 `json:"video_time"`
	DubTime   float64 `json:"dub_time"`
	Offset    float64 `json:"offset"`
	Score     float64 `json:"score"`
	Outlier   bool    `json:"outlier,omitempty"`
}

// SegmentKind identifies which source fills a part of the output timeline.
type SegmentKind string

const (
	Dub       SegmentKind = "dub"
	Reference SegmentKind = "reference"
	Silence   SegmentKind = "silence"
)

// Segment maps a video timeline interval to a source interval.
type Segment struct {
	Start       float64     `json:"start"`
	End         float64     `json:"end"`
	SourceStart float64     `json:"source_start"`
	Kind        SegmentKind `json:"kind"`
}

// Cut is an approximate middle cut in the Dream Cast dub.
type Cut struct {
	Start       float64 `json:"start"`
	End         float64 `json:"end"`
	Removed     float64 `json:"removed"`
	FilledFrom  string  `json:"filled_from"`
	SearchStart float64 `json:"search_start,omitempty"`
	SearchEnd   float64 `json:"search_end,omitempty"`
}

// Result describes the alignment decision.
type Result struct {
	Offset        float64   `json:"offset"`
	Confidence    float64   `json:"confidence"`
	DurationPrior float64   `json:"duration_prior"`
	VideoDuration float64   `json:"video_duration"`
	DubDuration   float64   `json:"dub_duration"`
	LeadGap       float64   `json:"lead_gap,omitempty"`
	TailGap       float64   `json:"tail_gap,omitempty"`
	Manual        bool      `json:"manual"`
	Uniform       bool      `json:"uniform"`
	Anchors       []Anchor  `json:"anchors"`
	Segments      []Segment `json:"segments"`
	Cuts          []Cut     `json:"cuts,omitempty"`
	Warnings      []string  `json:"warnings,omitempty"`
}

// DefaultAnchorTimes returns windows spread across the episode.
func DefaultAnchorTimes(videoDuration, window float64) []float64 {
	if videoDuration <= 0 {
		return nil
	}
	if window <= 0 {
		window = 30
	}
	var times []float64
	for t := 0.0; t+window <= videoDuration; t += 240 {
		times = append(times, t)
	}
	last := videoDuration - window - 1
	if last > 0 && (len(times) == 0 || last-times[len(times)-1] > 45) {
		times = append(times, last)
	}
	if len(times) == 0 {
		times = append(times, 0)
	}
	return times
}

// Align computes offsets and output segments. manualOffset is optional and
// uses the same convention as Offset (video time minus dub time).
func Align(dub *audio.Envelope, refs []RefWindow, videoDuration, dubDuration float64, manualOffset *float64) (*Result, error) {
	if dub == nil || len(dub.Values) < 100 {
		return nil, fmt.Errorf("дорожка Dream Cast слишком короткая для синхронизации")
	}
	if videoDuration <= 0 || dubDuration <= 0 {
		return nil, fmt.Errorf("неизвестна длительность видео или дубляжа")
	}
	res := &Result{
		VideoDuration: videoDuration,
		DubDuration:   dubDuration,
		DurationPrior: videoDuration - dubDuration,
		Uniform:       true,
	}
	if manualOffset != nil {
		res.Manual = true
		res.Offset = *manualOffset
		res.Confidence = 1
		res.Segments = []Segment{{
			Start:       0,
			End:         videoDuration,
			SourceStart: -*manualOffset,
			Kind:        Dub,
		}}
		return res, nil
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("нет референсных окон для синхронизации")
	}

	sort.Slice(refs, func(i, j int) bool { return refs[i].VideoTime < refs[j].VideoTime })
	dubEnv := dub.Values

	var anchors []Anchor
	prevOffset := 0.0
	havePrev := false
	for i, ref := range refs {
		if ref.Envelope == nil || len(ref.Envelope.Values) < 100 {
			continue
		}
		refEnv := ref.Envelope.Values
		var start float64
		var score float64
		if i == 0 {
			start, score = findGlobal(dubEnv, refEnv)
		} else {
			var centers []float64
			if havePrev {
				centers = append(centers, ref.VideoTime-prevOffset)
			}
			priorCenter := ref.VideoTime - res.DurationPrior
			centers = append(centers, priorCenter)
			start, score = findBestCenters(dubEnv, refEnv, centers, 30)
		}
		if start < 0 || score == 0 && start == 0 && i > 0 {
			continue
		}
		offset := ref.VideoTime - start
		anchors = append(anchors, Anchor{
			VideoTime: ref.VideoTime,
			DubTime:   start,
			Offset:    offset,
			Score:     score,
		})
		if score >= 0.18 {
			prevOffset = offset
			havePrev = true
		}
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("не удалось найти ни одного совпадения")
	}

	suppressOutliers(anchors)
	base, agreement, strength := chooseBase(anchors)
	res.Offset = base
	res.Confidence = 0.55*clamp01(strength) + 0.45*agreement
	if math.Abs(base-res.DurationPrior) <= 1.0 {
		res.Confidence = clamp01(res.Confidence + 0.03)
	}
	res.Anchors = anchors

	if cuts, segments := buildCuts(anchors, videoDuration, res.DurationPrior, base); len(cuts) > 0 {
		res.Cuts = cuts
		res.Segments = segments
		res.Uniform = false
		for _, c := range cuts {
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("обнаружен пропуск в дубляже около %s (%.1f c), заполнен оригиналом",
					formatTime(c.Start), c.End-c.Start))
		}
	} else {
		res.Segments = []Segment{{
			Start:       0,
			End:         videoDuration,
			SourceStart: -base,
			Kind:        Dub,
		}}
	}
	res.padEdges()
	if math.Abs(base-res.DurationPrior) > 1.5 && len(res.Cuts) == 0 && res.LeadGap == 0 && res.TailGap == 0 {
		res.Warnings = append(res.Warnings,
			fmt.Sprintf("offset (%.2f c) расходится с разницей длительностей (%.2f c)", base, res.DurationPrior))
	}
	return res, nil
}

// padEdges fills the parts of the video timeline that the dub cannot cover.
// When the dub starts after the video or ends before it, the missing edges are
// taken from the original reference track instead of silence.
func (r *Result) padEdges() {
	if len(r.Segments) == 0 {
		return
	}
	first := &r.Segments[0]
	if first.Kind == Dub && first.SourceStart < -0.02 {
		gap := -first.SourceStart
		if first.Start+gap >= first.End {
			*first = Segment{Start: first.Start, End: first.End, SourceStart: first.Start, Kind: Reference}
		} else {
			first.Start += gap
			first.SourceStart = 0
			r.Segments = append([]Segment{{
				Start: 0, End: first.Start, SourceStart: 0, Kind: Reference,
			}}, r.Segments...)
			r.LeadGap = gap
			r.Warnings = append(r.Warnings,
				fmt.Sprintf("дубляж начинается позже видео на %.1f c — начало заполнено оригиналом", gap))
		}
	}
	last := &r.Segments[len(r.Segments)-1]
	if last.End >= r.VideoDuration-0.01 && last.Kind == Dub {
		srcEnd := last.SourceStart + (last.End - last.Start)
		if srcEnd > r.DubDuration+0.02 {
			gap := srcEnd - r.DubDuration
			cutAt := last.End - gap
			if cutAt <= last.Start {
				*last = Segment{Start: last.Start, End: last.End, SourceStart: last.Start, Kind: Reference}
			} else {
				oldEnd := last.End
				last.End = cutAt
				r.Segments = append(r.Segments, Segment{
					Start: cutAt, End: oldEnd, SourceStart: cutAt, Kind: Reference,
				})
				r.TailGap = gap
				r.Warnings = append(r.Warnings,
					fmt.Sprintf("дубляж короче видео на %.1f c — конец заполнен оригиналом", gap))
			}
		}
	}
}

func findGlobal(dub, ref []float64) (float64, float64) {
	const factor = 10
	const coarseRate = audio.EnvelopeRate / factor
	dco := decimate(dub, factor)
	rco := decimate(ref, factor)
	if len(rco) < 5 || len(dco) <= len(rco) {
		return 0, 0
	}
	maxStart := float64(len(dco)-len(rco)) / coarseRate
	approx, score := searchAtRate(dco, rco, 0, maxStart, coarseRate)
	bestStart, bestScore := search(dub, ref, approx-0.4, approx+0.4)
	if bestScore == 0 {
		return approx, score
	}
	return bestStart, bestScore
}

func findBestCenters(dub, ref []float64, centers []float64, radius float64) (float64, float64) {
	var bestStart, bestScore float64
	found := false
	seen := make([]float64, 0, len(centers))
	for _, center := range centers {
		duplicate := false
		for _, c := range seen {
			if math.Abs(c-center) < 2 {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		seen = append(seen, center)
		start, score := search(dub, ref, center-radius, center+radius)
		if !found || score > bestScore {
			bestStart, bestScore = start, score
			found = true
		}
	}
	return bestStart, bestScore
}

// search performs normalized cross-correlation over starts in [centerLo, centerHi].
func search(dub, ref []float64, centerLo, centerHi float64) (float64, float64) {
	return searchAtRate(dub, ref, centerLo, centerHi, audio.EnvelopeRate)
}

func searchAtRate(dub, ref []float64, centerLo, centerHi, rate float64) (float64, float64) {
	n := len(ref)
	if n == 0 || len(dub) < n || rate <= 0 {
		return 0, 0
	}
	maxStart := len(dub) - n
	lo := int(math.Floor(centerLo * rate))
	hi := int(math.Ceil(centerHi * rate))
	if lo < 0 {
		lo = 0
	}
	if hi > maxStart {
		hi = maxStart
	}
	if lo > hi {
		return 0, 0
	}

	prefix := make([]float64, len(dub)+1)
	prefix2 := make([]float64, len(dub)+1)
	for i, v := range dub {
		prefix[i+1] = prefix[i] + v
		prefix2[i+1] = prefix2[i] + v*v
	}
	var refSum, refSum2 float64
	for _, v := range ref {
		refSum += v
		refSum2 += v * v
	}
	refMean := refSum / float64(n)
	refVar := refSum2/float64(n) - refMean*refMean
	if refVar < 1e-12 {
		return 0, 0
	}

	bestScore := math.Inf(-1)
	bestStart := lo
	fn := float64(n)
	for i := lo; i <= hi; i++ {
		var dot float64
		a := dub[i : i+n]
		for j, v := range ref {
			dot += a[j] * v
		}
		sum := prefix[i+n] - prefix[i]
		sum2 := prefix2[i+n] - prefix2[i]
		mean := sum / fn
		variance := sum2/fn - mean*mean
		if variance <= 1e-12 {
			continue
		}
		score := (dot - fn*mean*refMean) / (fn * math.Sqrt(variance*refVar))
		if score > bestScore {
			bestScore = score
			bestStart = i
		}
	}
	if math.IsInf(bestScore, -1) {
		return 0, 0
	}
	return float64(bestStart) / rate, bestScore
}

func decimate(x []float64, factor int) []float64 {
	if factor <= 1 {
		return x
	}
	n := len(x) / factor
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		var sum float64
		for j := 0; j < factor; j++ {
			sum += x[i*factor+j]
		}
		out[i] = sum / float64(factor)
	}
	return out
}

func suppressOutliers(anchors []Anchor) {
	for i := 1; i+1 < len(anchors); i++ {
		if anchors[i].Score < 0.18 {
			continue
		}
		if math.Abs(anchors[i].Offset-anchors[i-1].Offset) > 1.0 &&
			math.Abs(anchors[i].Offset-anchors[i+1].Offset) > 1.0 &&
			math.Abs(anchors[i-1].Offset-anchors[i+1].Offset) < 0.6 {
			anchors[i].Outlier = true
			anchors[i].Offset = (anchors[i-1].Offset + anchors[i+1].Offset) / 2
		}
	}
}

func chooseBase(anchors []Anchor) (offset, agreement, strength float64) {
	type cluster struct {
		mean        float64
		offsets     []float64
		weights     []float64
		weight      float64
		first, last float64
	}
	var clusters []cluster
	for _, a := range anchors {
		if a.Score < 0.18 {
			continue
		}
		w := math.Max(a.Score, 0.05)
		placed := false
		for i := range clusters {
			if math.Abs(clusters[i].mean-a.Offset) <= 0.6 {
				c := &clusters[i]
				c.offsets = append(c.offsets, a.Offset)
				c.weights = append(c.weights, w)
				c.weight += w
				c.mean += (a.Offset - c.mean) / float64(len(c.offsets))
				if a.VideoTime < c.first {
					c.first = a.VideoTime
				}
				if a.VideoTime > c.last {
					c.last = a.VideoTime
				}
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, cluster{
				mean:    a.Offset,
				offsets: []float64{a.Offset},
				weights: []float64{w},
				weight:  w,
				first:   a.VideoTime,
				last:    a.VideoTime,
			})
		}
	}
	if len(clusters) == 0 {
		for _, a := range anchors {
			offset += a.Offset
		}
		offset /= float64(len(anchors))
		return offset, 0, 0
	}
	best := clusters[0]
	for _, c := range clusters[1:] {
		if c.weight > best.weight {
			best = c
		}
	}
	// The base is the offset at the start of the episode. A cut can only
	// increase the offset later, so prefer the lowest well supported state.
	// False correlation peaks are short lived: require the state to span at
	// least 20 seconds before it can be the opening offset.
	threshold := 0.4 * best.weight
	baseCluster := best
	for _, c := range clusters {
		if c.mean >= baseCluster.mean {
			continue
		}
		if c.weight < threshold || c.last-c.first < 20 {
			continue
		}
		baseCluster = c
	}
	best = baseCluster
	// The weighted median is robust to a few poor matches that still land
	// inside the cluster (they must not drag the whole episode by ~0.1 s).
	offset = weightedMedian(best.offsets, best.weights)
	var sum, wsum float64
	for i, o := range best.offsets {
		if math.Abs(o-offset) <= 0.25 {
			sum += o * best.weights[i]
			wsum += best.weights[i]
		}
	}
	if wsum > 0 {
		offset = sum / wsum
	}
	valid := 0
	matching := 0
	for _, a := range anchors {
		if a.Score < 0.15 {
			continue
		}
		valid++
		if math.Abs(a.Offset-offset) <= 1.0 {
			matching++
		}
	}
	if valid > 0 {
		agreement = float64(matching) / float64(valid)
	}
	scores := make([]float64, 0, len(anchors))
	for _, a := range anchors {
		scores = append(scores, math.Max(0, a.Score))
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(scores)))
	for i := 0; i < len(scores) && i < 3; i++ {
		strength += scores[i]
	}
	if n := math.Min(3, float64(len(scores))); n > 0 {
		strength /= n
	}
	return offset, agreement, strength
}

func weightedMedian(values, weights []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	type item struct {
		v, w float64
	}
	items := make([]item, len(values))
	var total float64
	for i := range values {
		items[i] = item{values[i], weights[i]}
		total += weights[i]
	}
	sort.Slice(items, func(i, j int) bool { return items[i].v < items[j].v })
	var acc float64
	for _, it := range items {
		acc += it.w
		if acc >= total/2 {
			return it.v
		}
	}
	return items[len(items)-1].v
}

func buildCuts(anchors []Anchor, videoDuration, durationPrior, base float64) ([]Cut, []Segment) {
	valid := make([]Anchor, 0, len(anchors))
	for _, a := range anchors {
		if a.Score >= 0.18 {
			valid = append(valid, a)
		}
	}
	if len(valid) < 2 {
		return nil, nil
	}

	// How much material can the dub possibly have lost? The total offset
	// growth is bounded by the duration difference (video - dub): the dub
	// cannot skip more than it is shorter than the video. A small allowance
	// covers container duration rounding.
	maxRemoved := durationPrior - base + 0.5
	if maxRemoved < 0 {
		maxRemoved = 0
	}

	const tol = 1.0
	type group struct {
		offset      float64
		first, last float64
		count       int
	}
	groups := []group{{offset: base}}
	lastInlier := math.Inf(-1)
	removedTotal := 0.0

	for i, a := range valid {
		diff := a.Offset - groups[len(groups)-1].offset
		if math.Abs(diff) <= tol {
			g := &groups[len(groups)-1]
			if g.count == 0 {
				g.first = a.VideoTime
			}
			g.last = a.VideoTime
			g.count++
			lastInlier = a.VideoTime
			continue
		}
		if diff < -tol {
			// Offsets only grow; a backwards jump is a false match.
			continue
		}
		// A forward jump is a possible cut. It has to be physically possible:
		// a cut can never remove more dub than the time elapsed since the last
		// confirmed anchor.
		gap := a.VideoTime - lastInlier
		if !math.IsInf(lastInlier, -1) && diff > gap+1.5 {
			continue
		}
		// Do not accept a jump that would remove more material than the
		// duration discrepancy allows.
		if removedTotal+diff > maxRemoved {
			continue
		}
		// A cut has to be sustained: at least two later anchors must agree
		// with the new offset, unless the new offset matches the duration
		// prior (the post-cut state at the end of the episode).
		confirm := 0
		for j := i + 1; j < len(valid); j++ {
			b := valid[j]
			if b.VideoTime > a.VideoTime+600 {
				break
			}
			if math.Abs(b.Offset-a.Offset) <= 1.5 {
				confirm++
				if confirm >= 2 {
					break
				}
			}
		}
		priorMatch := math.Abs(a.Offset-durationPrior) <= 1.5
		if confirm < 2 && !priorMatch {
			continue
		}
		removedTotal += diff
		groups = append(groups, group{offset: a.Offset, first: a.VideoTime, last: a.VideoTime, count: 1})
		lastInlier = a.VideoTime
	}
	if len(groups) < 2 {
		return nil, nil
	}
	// Drop a leading group that never got an anchor (the base may have been
	// replaced by a later, better supported offset).
	clean := groups[:0]
	for _, g := range groups {
		if g.count > 0 {
			clean = append(clean, g)
		}
	}
	groups = clean
	if len(groups) < 2 {
		return nil, nil
	}

	var cuts []Cut
	var segments []Segment
	cursor := 0.0
	offset := groups[0].offset
	if groups[0].first > 0.01 {
		segments = append(segments, Segment{Start: 0, End: groups[0].first, SourceStart: -offset, Kind: Dub})
		cursor = groups[0].first
	}
	for i := 1; i < len(groups); i++ {
		prev := groups[i-1]
		next := groups[i]
		anchorStart := prev.last
		anchorEnd := next.first
		if anchorStart < cursor {
			anchorStart = cursor
		}
		if anchorEnd > videoDuration {
			anchorEnd = videoDuration
		}
		removed := next.offset - offset
		if removed <= 0.05 {
			offset = next.offset
			continue
		}
		// The actual cut lies somewhere in [anchorStart, anchorEnd-removed].
		// Place the filled gap in the middle of that uncertainty interval so
		// that we replace roughly the removed duration, not the whole anchor gap.
		uncertain := anchorEnd - removed - anchorStart
		fillStart := anchorStart
		if uncertain > 0 {
			fillStart += uncertain / 2
		}
		fillEnd := fillStart + removed
		if fillEnd > anchorEnd {
			fillEnd = anchorEnd
		}
		if fillEnd-fillStart > 0.05 {
			segments = append(segments, Segment{Start: cursor, End: fillStart, SourceStart: -offset, Kind: Dub})
			segments = append(segments, Segment{Start: fillStart, End: fillEnd, SourceStart: fillStart, Kind: Reference})
			cuts = append(cuts, Cut{
				Start:       fillStart,
				End:         fillEnd,
				Removed:     fillEnd - fillStart,
				FilledFrom:  "original",
				SearchStart: anchorStart,
				SearchEnd:   anchorEnd,
			})
			cursor = fillEnd
		}
		offset = next.offset
	}
	if cursor < videoDuration {
		segments = append(segments, Segment{Start: cursor, End: videoDuration, SourceStart: -offset, Kind: Dub})
	}
	// Drop zero-length segments.
	cleaned := segments[:0]
	for _, s := range segments {
		if s.End-s.Start > 0.01 {
			cleaned = append(cleaned, s)
		}
	}
	return cuts, cleaned
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func formatTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	h := int(sec) / 3600
	m := (int(sec) % 3600) / 60
	s := int(sec) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// Summary renders a compact human-readable report.
func (r *Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "offset: %+.2f c\n", r.Offset)
	fmt.Fprintf(&b, "confidence: %.2f\n", r.Confidence)
	fmt.Fprintf(&b, "duration prior: %+.2f c\n", r.DurationPrior)
	if r.Manual {
		b.WriteString("manual offset\n")
	}
	if len(r.Cuts) > 0 {
		fmt.Fprintf(&b, "cuts: %d\n", len(r.Cuts))
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	return b.String()
}
