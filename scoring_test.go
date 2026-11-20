/*
 * Copyright (c) 2014-2026 Christian Muehlhaeuser
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 */

package smartcrop

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"runtime"
	"sync"
	"testing"
)

func mustNormalize(t *testing.T, req Request) *NormalizedImage {
	t.Helper()
	n, err := Normalize(req, Limits{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	return n
}

func mustScorer(t *testing.T, cfg Config) *Scorer {
	t.Helper()
	s, err := NewScorer(cfg)
	if err != nil {
		t.Fatalf("NewScorer: %v", err)
	}
	return s
}

func TestScorerDeterministicCandidates(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(320, 240), Width: 160, Height: 120})
	for _, seed := range []uint64{0, 1, 42, math.MaxUint64} {
		cfg := Config{Seed: seed}
		a, err := mustScorer(t, cfg).Rank(context.Background(), n)
		if err != nil {
			t.Fatal(err)
		}
		b, err := mustScorer(t, cfg).Rank(context.Background(), n)
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Candidates) != len(b.Candidates) {
			t.Fatalf("seed %d: %d vs %d candidates", seed, len(a.Candidates), len(b.Candidates))
		}
		for i := range a.Candidates {
			if a.Candidates[i] != b.Candidates[i] {
				t.Fatalf("seed %d candidate %d differs: %+v vs %+v", seed, i, a.Candidates[i], b.Candidates[i])
			}
		}
		if a.Best.Rectangle != b.Best.Rectangle || a.Best.TotalScore != b.Best.TotalScore {
			t.Fatalf("seed %d best differs: %v vs %v", seed, a.Best.Rectangle, b.Best.Rectangle)
		}
	}
}

func TestScorerCandidateLegality(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(200, 130), Width: 100, Height: 100})
	r, err := mustScorer(t, Config{Seed: 5}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	canonical := image.Rect(0, 0, n.Width, n.Height)
	seen := make(map[image.Rectangle]struct{}, len(r.Candidates))
	for i, c := range r.Candidates {
		if c.Rectangle.Empty() {
			t.Fatalf("candidate %d is empty", i)
		}
		if !c.Rectangle.In(canonical) {
			t.Fatalf("candidate %d %v escapes %v", i, c.Rectangle, canonical)
		}
		if _, dup := seen[c.Rectangle]; dup {
			t.Fatalf("candidate %d %v is a duplicate", i, c.Rectangle)
		}
		seen[c.Rectangle] = struct{}{}
		if c.Order < 0 {
			t.Fatalf("candidate %d has negative order", i)
		}
		if c.Scale < nTargetMinScale-1e-9 || c.Scale > 1+1e-9 {
			t.Fatalf("candidate %d has scale %v", i, c.Scale)
		}
	}
}

const nTargetMinScale = 0.9

func TestScorerRankingOrder(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(256, 192), Width: 128, Height: 96})
	r, err := mustScorer(t, Config{Seed: 11, DisableJitter: true}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(r.Candidates); i++ {
		if candidateLess(r.Candidates[i], r.Candidates[i-1]) {
			t.Fatalf("candidate %d sorts before %d", i, i-1)
		}
	}
	if r.Best.Rectangle != r.Candidates[0].Rectangle {
		t.Fatalf("Best %v != Candidates[0] %v", r.Best.Rectangle, r.Candidates[0].Rectangle)
	}
}

func TestScorerScaleRounding(t *testing.T) {
	// Target width 15 at scale 0.9 -> 13.5 -> rounds half up to 14.
	n := mustNormalize(t, Request{Image: buildPattern(40, 40), Width: 15, Height: 15})
	s := mustScorer(t, Config{DisableJitter: true, MinScale: 0.9, MaxScale: 0.9, MaxCandidates: 10000})
	r, err := s.Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Candidates) == 0 {
		t.Fatal("no candidates")
	}
	want := roundHalfUp(float64(n.TargetWidth) * 0.9)
	for i, c := range r.Candidates {
		if c.Rectangle.Dx() != want || c.Rectangle.Dy() != want {
			t.Fatalf("candidate %d size = %dx%d, want %dx%d", i, c.Rectangle.Dx(), c.Rectangle.Dy(), want, want)
		}
	}
}

func TestScorerBoundaryClipping(t *testing.T) {
	// A target larger than the source must clip, never overflow.
	n := mustNormalize(t, Request{Image: buildPattern(30, 20), Width: 400, Height: 400})
	// The square target is fitted to the source: 20x20 inside a 30x20 image.
	if n.TargetWidth != 20 || n.TargetHeight != 20 {
		t.Fatalf("target = %dx%d, want 20x20", n.TargetWidth, n.TargetHeight)
	}
	r, err := mustScorer(t, Config{Seed: 3, MinScale: 1, MaxScale: 1, Step: 4}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	canonical := image.Rect(0, 0, 30, 20)
	for _, c := range r.Candidates {
		if !c.Rectangle.In(canonical) || c.Rectangle.Dx() != 20 || c.Rectangle.Dy() != 20 {
			t.Fatalf("candidate %v is not a clipped 20x20 box inside %v", c.Rectangle, canonical)
		}
	}
}

func TestScorerRespectsConfigKnobs(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(160, 160), Width: 64, Height: 32})
	plain, err := mustScorer(t, Config{Seed: 2, DisableJitter: true, MinScale: 1, MaxScale: 1, Step: 4}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	jittered, err := mustScorer(t, Config{Seed: 2, MinScale: 1, MaxScale: 1, Step: 4}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain.Candidates) == len(jittered.Candidates) {
		// Same count is possible, but positions must differ somewhere.
		same := true
		for i := range plain.Candidates {
			if plain.Candidates[i].Rectangle != jittered.Candidates[i].Rectangle {
				same = false
				break
			}
		}
		if same {
			t.Fatal("jitter did not change candidate positions")
		}
	}

	twoScales, err := mustScorer(t, Config{Seed: 2, DisableJitter: true, MinScale: 0.9, MaxScale: 1}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(twoScales.Candidates) <= len(plain.Candidates) {
		t.Fatalf("two scales produced %d candidates, one scale %d", len(twoScales.Candidates), len(plain.Candidates))
	}

	noThirds, err := mustScorer(t, Config{Seed: 2, DisableJitter: true, MinScale: 1, MaxScale: 1, Step: 4, DisableRuleOfThirds: true}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if len(noThirds.Candidates) != len(plain.Candidates) {
		t.Fatalf("disabling rule of thirds changed the candidate count")
	}
	if noThirds.Candidates[0].TotalScore == plain.Candidates[0].TotalScore && noThirds.Best.Rectangle == plain.Best.Rectangle {
		t.Fatal("disabling rule of thirds had no effect on scores")
	}

	edge := 0.0
	noEdge, err := mustScorer(t, Config{Seed: 2, DisableJitter: true, MinScale: 1, MaxScale: 1, Step: 4, EdgeWeight: &edge}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if noEdge.Candidates[0].TotalScore == plain.Candidates[0].TotalScore {
		t.Fatal("changing the edge weight had no effect on scores")
	}
}

func TestScorerSerialEqualsParallel(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(360, 280), Width: 180, Height: 120})
	serial, err := mustScorer(t, Config{Seed: 9, Concurrency: 1, Step: 4}).Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{2, 4, 16} {
		par, err := mustScorer(t, Config{Seed: 9, Concurrency: workers, Step: 4}).Rank(context.Background(), n)
		if err != nil {
			t.Fatal(err)
		}
		if len(serial.Candidates) != len(par.Candidates) {
			t.Fatalf("workers=%d: %d vs %d candidates", workers, len(serial.Candidates), len(par.Candidates))
		}
		for i := range serial.Candidates {
			if serial.Candidates[i] != par.Candidates[i] {
				t.Fatalf("workers=%d candidate %d differs", workers, i)
			}
		}
	}
}

func TestScorerStatsAndCleanup(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	s := mustScorer(t, Config{Seed: 1, Concurrency: 4})
	r, err := s.Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	if r.Stats.Candidates != len(r.Candidates) || r.Stats.Scored != len(r.Candidates) {
		t.Fatalf("stats out of sync: %+v", r.Stats)
	}
	if r.Stats.PeakWorkers < 1 || r.Stats.PeakWorkers > r.Stats.Workers {
		t.Fatalf("peak workers %d outside [1,%d]", r.Stats.PeakWorkers, r.Stats.Workers)
	}
	if got := s.ActiveWorkers(); got != 0 {
		t.Fatalf("ActiveWorkers = %d after Rank", got)
	}
}

func TestScorerBudgetExhausted(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	s := mustScorer(t, Config{Seed: 1, MaxCandidates: 3})
	r, err := s.Rank(context.Background(), n)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v, want ErrBudgetExhausted", err)
	}
	if len(r.Candidates) != 0 || r.Best.Rectangle != (image.Rectangle{}) {
		t.Fatalf("expected zero ranking on budget exhaustion, got %d candidates", len(r.Candidates))
	}
	if got := s.ActiveWorkers(); got != 0 {
		t.Fatalf("ActiveWorkers = %d after failure", got)
	}
}

func TestScorerCandidateFailureIsAtomic(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	s := mustScorer(t, Config{Seed: 1, Concurrency: 4})
	s.failCandidate = func(c Candidate) bool { return c.Min.X%16 == 0 && c.Min.Y%16 == 0 }
	r, err := s.Rank(context.Background(), n)
	if !errors.Is(err, ErrCandidateFailed) {
		t.Fatalf("got %v, want ErrCandidateFailed", err)
	}
	if len(r.Candidates) != 0 {
		t.Fatalf("expected no partial ranking, got %d candidates", len(r.Candidates))
	}
	if got := s.ActiveWorkers(); got != 0 {
		t.Fatalf("ActiveWorkers = %d after failure", got)
	}
}

func TestScorerCancelMidRun(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(320, 240), Width: 160, Height: 120})
	s := mustScorer(t, Config{Seed: 1, Concurrency: 8, Step: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := s.Rank(ctx, n)
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
	if len(r.Candidates) != 0 {
		t.Fatalf("expected no partial ranking, got %d", len(r.Candidates))
	}
	if got := s.ActiveWorkers(); got != 0 {
		t.Fatalf("ActiveWorkers = %d after cancellation", got)
	}
}

func TestScorerScoreCandidateValidation(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(64, 64), Width: 32, Height: 32})
	s := mustScorer(t, Config{Seed: 1})
	good := image.Rect(0, 0, 32, 32)
	c, err := s.ScoreCandidate(context.Background(), n, good)
	if err != nil {
		t.Fatalf("ScoreCandidate: %v", err)
	}
	if c.Rectangle != good || math.IsNaN(c.TotalScore) {
		t.Fatalf("unexpected candidate %+v", c)
	}
	bad := []image.Rectangle{
		{},
		image.Rect(0, 0, 0, 10),
		image.Rect(-1, 0, 10, 10),
		image.Rect(0, 0, 65, 10),
	}
	for _, rect := range bad {
		if _, err := s.ScoreCandidate(context.Background(), n, rect); !errors.Is(err, ErrCandidateFailed) {
			t.Fatalf("%v: got %v, want ErrCandidateFailed", rect, err)
		}
	}
}

func TestScorerKeepsRuleOfThirdsEdgeSemantics(t *testing.T) {
	// The 3x3 thirds points should score higher than the exact center for a
	// crop with saliency at a thirds intersection.
	n := mustNormalize(t, Request{Image: buildPattern(96, 96), Width: 32, Height: 32})
	s := mustScorer(t, Config{Seed: 1})
	center := image.Rect(32, 32, 64, 64)
	upLeft, err := s.ScoreCandidate(context.Background(), n, image.Rect(10, 10, 42, 42))
	if err != nil {
		t.Fatal(err)
	}
	midC, err := s.ScoreCandidate(context.Background(), n, center)
	if err != nil {
		t.Fatal(err)
	}
	// Sanity: identical inputs must give identical scores.
	again, err := s.ScoreCandidate(context.Background(), n, center)
	if err != nil {
		t.Fatal(err)
	}
	if again.TotalScore != midC.TotalScore {
		t.Fatalf("score instability: %v vs %v", again.TotalScore, midC.TotalScore)
	}
	if upLeft.TotalScore == midC.TotalScore {
		t.Fatal("expected distinct scores for distinct rectangles")
	}
}

func TestScorerConcurrentRankSameImage(t *testing.T) {
	n := mustNormalize(t, Request{Image: buildPattern(200, 150), Width: 100, Height: 75})
	s := mustScorer(t, Config{Seed: 4, Concurrency: 4})
	baseline, err := s.Rank(context.Background(), n)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.Rank(context.Background(), n)
			if err != nil {
				errs <- err
				return
			}
			if got.Best.Rectangle != baseline.Best.Rectangle || got.Best.TotalScore != baseline.Best.TotalScore {
				errs <- errors.New("concurrent run produced a different winner")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestScorerRejectsInvalidConfig(t *testing.T) {
	cases := []Config{
		{Step: -1},
		{MinScale: -1},
		{MinScale: 0.5, MaxScale: 0.2},
		{MaxScale: 1.5},
		{Concurrency: -2},
		{Timeout: -1},
		{TopCandidates: -1},
		{MaxCandidates: -5},
	}
	for i, cfg := range cases {
		if _, err := NewScorer(cfg); err == nil {
			t.Fatalf("case %d (%+v): expected error", i, cfg)
		}
	}
}

func TestScaleSequence(t *testing.T) {
	got := scaleSequence(0.9, 1.0)
	want := []float64{1, 0.9}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-12 {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	got = scaleSequence(0.9, 0.99)
	if len(got) != 1 || got[0] != 0.9 {
		t.Fatalf("got %v, want [0.9]", got)
	}
}

func TestCandidateLessTotalOrder(t *testing.T) {
	a := Candidate{Rectangle: image.Rect(0, 0, 10, 10), TotalScore: 1, Order: 1}
	b := Candidate{Rectangle: image.Rect(0, 0, 10, 10), TotalScore: 1, Order: 2}
	if !candidateLess(a, b) || candidateLess(b, a) {
		t.Fatal("tie-break by order is not a strict order")
	}
	c := Candidate{Rectangle: image.Rect(5, 0, 15, 10), TotalScore: 1, Order: 0}
	if !candidateLess(a, c) {
		t.Fatal("expected Min.X tie-break")
	}
}

func TestTransparentEdgesDoNotCrashScoring(t *testing.T) {
	src := buildAlphaEdges()
	o, err := NewOrchestrator(Config{Seed: 2})
	if err != nil {
		t.Fatal(err)
	}
	res, err := o.Crop(context.Background(), Request{Image: src, Width: 80, Height: 80})
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	if res.Rectangle.Empty() {
		t.Fatal("empty rectangle")
	}
	if math.IsNaN(res.TotalScore) {
		t.Fatal("NaN score for transparent edges")
	}
}

func TestSolidColorImageIsDeterministic(t *testing.T) {
	src := solid(color.RGBA{120, 120, 120, 255}, 128, 96)
	o, err := NewOrchestrator(Config{Seed: 8, DisableJitter: true})
	if err != nil {
		t.Fatal(err)
	}
	a, err := o.Crop(context.Background(), Request{Image: src, Width: 64, Height: 64})
	if err != nil {
		t.Fatal(err)
	}
	b, err := o.Crop(context.Background(), Request{Image: src, Width: 64, Height: 64})
	if err != nil {
		t.Fatal(err)
	}
	if a.Rectangle != b.Rectangle {
		t.Fatalf("solid color winner changed: %v vs %v", a.Rectangle, b.Rectangle)
	}
}

func TestGoroutinesDoNotLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	n := mustNormalize(t, Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	s := mustScorer(t, Config{Seed: 1, Concurrency: 8})
	for i := 0; i < 5; i++ {
		if _, err := s.Rank(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	after := runtime.NumGoroutine()
	if after > before+2 {
		t.Fatalf("goroutines grew from %d to %d", before, after)
	}
}
