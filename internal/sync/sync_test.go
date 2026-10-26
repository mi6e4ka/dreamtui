package sync

import (
	"math"
	"math/rand"
	"strings"
	"testing"

	"dreamtui/internal/audio"
)

func makeEnvelope(values []float64) *audio.Envelope {
	return &audio.Envelope{Values: values, Duration: float64(len(values)) / audio.EnvelopeRate}
}

// smoothSignal is a deterministic, positive envelope with structure at many
// time scales, similar to real music/effects side audio.
func smoothSignal(seconds float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	n := int(seconds * audio.EnvelopeRate)
	out := make([]float64, n)
	var v float64
	for i := range out {
		v = 0.92*v + 0.08*rng.Float64()
		out[i] = 0.02 + v + 0.015*math.Sin(float64(i)/37)
	}
	return out
}

func sliceEnv(src []float64, start, seconds float64) []float64 {
	lo := int(start * audio.EnvelopeRate)
	hi := lo + int(seconds*audio.EnvelopeRate)
	out := make([]float64, hi-lo)
	copy(out, src[lo:hi])
	return out
}

func TestAlignUniform(t *testing.T) {
	const offset = -6.61
	videoDur := 400.0
	dub := smoothSignal(videoDur-offset, 7)
	video := smoothSignal(videoDur, 11)
	// Video reference is the master timeline; the dub is delayed by -offset.
	var refs []RefWindow
	for _, vt := range []float64{20, 120, 220, 320} {
		refs = append(refs, RefWindow{
			VideoTime: vt,
			Envelope:  makeEnvelope(sliceEnv(dub, vt-offset, 30)),
		})
	}
	res, err := Align(makeEnvelope(dub), refs, videoDur, float64(len(dub))/audio.EnvelopeRate, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = video
	if math.Abs(res.Offset-offset) > 0.05 {
		t.Fatalf("offset = %.3f, want %.3f", res.Offset, offset)
	}
	if res.Confidence < 0.9 {
		t.Fatalf("confidence = %.3f", res.Confidence)
	}
	if !res.Uniform || len(res.Cuts) != 0 {
		t.Fatalf("unexpected cuts: %+v", res.Cuts)
	}
}

func TestAlignDetectsCut(t *testing.T) {
	const intro = 6.6
	const cutLen = 5.0
	masterDur := 406.6
	videoDur := 400.0
	master := smoothSignal(masterDur, 21)
	// The dub drops master[200:205].
	dub := append([]float64{}, master[:int(200*audio.EnvelopeRate)]...)
	dub = append(dub, master[int(205*audio.EnvelopeRate):]...)

	// Video t -> master t+intro. Reference windows come from the master.
	ref := func(vt float64) RefWindow {
		return RefWindow{VideoTime: vt, Envelope: makeEnvelope(sliceEnv(master, vt+intro, 30))}
	}
	refs := []RefWindow{ref(30), ref(120), ref(210), ref(330)}
	dubDur := float64(len(dub)) / audio.EnvelopeRate
	res, err := Align(makeEnvelope(dub), refs, videoDur, dubDur, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cuts) != 1 {
		t.Fatalf("cuts = %+v, offset %.2f", res.Cuts, res.Offset)
	}
	if res.Uniform {
		t.Fatal("result marked uniform")
	}
	if math.Abs(res.Cuts[0].Removed-cutLen) > 0.2 {
		t.Fatalf("removed = %.2f, want %.2f", res.Cuts[0].Removed, cutLen)
	}
	// Before the cut the offset is -intro; after it is -(intro-cutLen):
	// the dub resumes earlier relative to the video, so offset grows by cutLen.
	if math.Abs(res.Offset+intro) > 0.05 && math.Abs(res.Offset+intro-cutLen) > 0.05 {
		t.Fatalf("offset = %.3f", res.Offset)
	}
	var covered float64
	var first *Segment
	for i := range res.Segments {
		s := res.Segments[i]
		if s.End <= s.Start {
			t.Fatalf("bad segment %+v", s)
		}
		if first == nil {
			first = &s
		}
		covered += s.End - s.Start
	}
	if math.Abs(covered-videoDur) > 0.01 {
		t.Fatalf("segments cover %.2f of %.2f", covered, videoDur)
	}
	if first == nil || first.Start != 0 || math.Abs(first.SourceStart-intro) > 0.05 {
		t.Fatalf("first segment = %+v, want start 0 source %.2f", first, intro)
	}
}

// TestAlignIgnoresLeadingFalseMatch reproduces an episode where the opening
// reference window has a stronger match near the end of the dub (repeated
// music/recap). The first anchor then reports a huge negative offset; that
// must not become a cut at 00:00 that shifts the whole track.
func TestAlignIgnoresLeadingFalseMatch(t *testing.T) {
	const offset = -5.53
	const videoDur = 1377.168
	const dubDur = 1381.807 // duration prior = -4.639
	dub := smoothSignal(dubDur, 53)
	var refs []RefWindow
	// The first window is a false match: it is a copy of the dub near its end.
	refs = append(refs, RefWindow{
		VideoTime: 0,
		Envelope:  makeEnvelope(sliceEnv(dub, 1285.11, 30)),
	})
	for _, vt := range []float64{240, 480, 720, 960, 1200, 1366} {
		start := vt - offset
		if start+30 > dubDur {
			continue
		}
		refs = append(refs, RefWindow{VideoTime: vt, Envelope: makeEnvelope(sliceEnv(dub, start, 30))})
	}
	res, err := Align(makeEnvelope(dub), refs, videoDur, dubDur, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.Offset-offset) > 0.1 {
		t.Fatalf("offset = %.3f, want %.2f", res.Offset, offset)
	}
	if len(res.Cuts) != 0 {
		t.Fatalf("false cuts detected: %+v", res.Cuts)
	}
	if !res.Uniform {
		t.Fatal("result marked non-uniform")
	}
	if res.LeadGap != 0 {
		t.Fatalf("unexpected lead gap %.2f", res.LeadGap)
	}
	if res.Segments[0].Kind != Dub || math.Abs(res.Segments[0].SourceStart-5.53) > 0.1 {
		t.Fatalf("first segment = %+v, want dub from its intro", res.Segments[0])
	}
	var covered float64
	for _, s := range res.Segments {
		covered += s.End - s.Start
	}
	if math.Abs(covered-videoDur) > 0.01 {
		t.Fatalf("segments cover %.2f of %.2f", covered, videoDur)
	}
}

// TestBuildCutsRejectsImpossibleLeadingGroup checks the duration guard: a dub
// cannot lose 16 s when its duration differs from the video by less than 1 s.
func TestBuildCutsRejectsImpossibleLeadingGroup(t *testing.T) {
	anchors := []Anchor{
		{VideoTime: 0, Offset: -1285.11, Score: 0.58},
		{VideoTime: 8, Offset: -15.14, Score: 0.62},
		{VideoTime: 16, Offset: -5.53, Score: 0.56},
		{VideoTime: 24, Offset: -5.53, Score: 0.56},
		{VideoTime: 240, Offset: -5.53, Score: 0.77},
		{VideoTime: 480, Offset: -5.53, Score: 0.85},
		{VideoTime: 720, Offset: -5.53, Score: 0.77},
		{VideoTime: 1200, Offset: -5.53, Score: 0.82},
		{VideoTime: 1346, Offset: -5.53, Score: 0.85},
	}
	cuts, segments := buildCuts(anchors, 1377.168, -4.639, -5.53)
	if len(cuts) != 0 || segments != nil {
		t.Fatalf("cuts = %+v, segments = %+v", cuts, segments)
	}
}

// TestBuildCutsKeepsGenuineCut guards against over-filtering: a real 5 s cut
// is still found even when a false leading group is present.
func TestBuildCutsKeepsGenuineCut(t *testing.T) {
	anchors := []Anchor{
		{VideoTime: 0, Offset: -772.4, Score: 0.50},
		{VideoTime: 8, Offset: -766.7, Score: 0.63},
		{VideoTime: 30, Offset: -6.6, Score: 0.95},
		{VideoTime: 120, Offset: -6.6, Score: 0.95},
		{VideoTime: 210, Offset: -1.6, Score: 0.95},
		{VideoTime: 330, Offset: -1.6, Score: 0.95},
	}
	cuts, segments := buildCuts(anchors, 400, -1.6, -6.6)
	if len(cuts) != 1 {
		t.Fatalf("cuts = %+v", cuts)
	}
	if math.Abs(cuts[0].Removed-5) > 0.2 {
		t.Fatalf("removed = %.2f, want 5", cuts[0].Removed)
	}
	if len(segments) < 2 || segments[0].Kind != Dub || segments[0].Start != 0 {
		t.Fatalf("segments = %+v", segments)
	}
}

func TestAlignPadsTailWithOriginal(t *testing.T) {
	const offset = -6.6
	const videoDur = 400.0
	const dubDur = 386.0
	dub := smoothSignal(dubDur, 31)
	var refs []RefWindow
	for _, vt := range []float64{20, 120, 220, 320} {
		start := vt - offset
		if start+30 > dubDur {
			continue
		}
		refs = append(refs, RefWindow{VideoTime: vt, Envelope: makeEnvelope(sliceEnv(dub, start, 30))})
	}
	res, err := Align(makeEnvelope(dub), refs, videoDur, dubDur, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.Offset-offset) > 0.05 {
		t.Fatalf("offset = %.3f, want %.2f", res.Offset, offset)
	}
	if math.Abs(res.TailGap-20.6) > 0.2 {
		t.Fatalf("tail gap = %.2f, want 20.6", res.TailGap)
	}
	last := res.Segments[len(res.Segments)-1]
	if last.Kind != Reference || math.Abs(last.End-videoDur) > 0.01 {
		t.Fatalf("last segment = %+v, want reference ending at %.0f", last, videoDur)
	}
	var covered float64
	for _, s := range res.Segments {
		covered += s.End - s.Start
	}
	if math.Abs(covered-videoDur) > 0.01 {
		t.Fatalf("segments cover %.2f of %.2f", covered, videoDur)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "расходится") {
			t.Fatalf("edge gap should explain the duration difference: %v", res.Warnings)
		}
	}
}

func TestAlignPadsLeadWithOriginal(t *testing.T) {
	const offset = 5.0
	const videoDur = 400.0
	dub := smoothSignal(videoDur, 41)
	var refs []RefWindow
	for _, vt := range []float64{20, 120, 220, 320} {
		start := vt - offset
		if start < 0 || start+30 > videoDur {
			continue
		}
		refs = append(refs, RefWindow{VideoTime: vt, Envelope: makeEnvelope(sliceEnv(dub, start, 30))})
	}
	res, err := Align(makeEnvelope(dub), refs, videoDur, videoDur, nil)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.Offset-offset) > 0.05 {
		t.Fatalf("offset = %.3f, want %.2f", res.Offset, offset)
	}
	if math.Abs(res.LeadGap-offset) > 0.05 {
		t.Fatalf("lead gap = %.2f, want %.2f", res.LeadGap, offset)
	}
	first := res.Segments[0]
	if first.Kind != Reference || first.Start != 0 || math.Abs(first.End-offset) > 0.05 {
		t.Fatalf("first segment = %+v, want reference 0..%.0f", first, offset)
	}
}
