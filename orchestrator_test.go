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
	"sort"
	"sync"
	"testing"
	"time"
)

func mustOrchestrator(t *testing.T, cfg Config) *Orchestrator {
	t.Helper()
	o, err := NewOrchestrator(cfg)
	if err != nil {
		t.Fatalf("NewOrchestrator: %v", err)
	}
	return o
}

func TestOrchestratorCropAndCache(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 7})
	req := Request{Image: buildPattern(240, 180), Width: 120, Height: 90, Metadata: "café-猫"}

	first, err := o.Crop(context.Background(), req)
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	if first.CacheHit {
		t.Fatal("first call must be a cache miss")
	}
	second, err := o.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit {
		t.Fatal("second call must be a cache hit")
	}
	if first.Digest != second.Digest || first.Rectangle != second.Rectangle {
		t.Fatalf("cache returned a different result: %s vs %s", first.Summary(), second.Summary())
	}
	if o.CacheLen() != 1 {
		t.Fatalf("cache holds %d entries, want 1", o.CacheLen())
	}
	o.ClearCache()
	if o.CacheLen() != 0 {
		t.Fatalf("ClearCache left %d entries", o.CacheLen())
	}
}

// mutatedPattern is buildPattern with one pixel changed.
func mutatedPattern(w, h int) image.Image {
	img := buildPattern(w, h)
	img.SetRGBA(w/2, h/2, color.RGBA{1, 2, 3, 255})
	return img
}

func TestOrchestratorCacheKeySensitivity(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 7})
	src := buildPattern(160, 120)
	base := Request{Image: src, Width: 80, Height: 80}
	if _, err := o.Crop(context.Background(), base); err != nil {
		t.Fatal(err)
	}

	variants := []Request{
		{Image: src, Width: 80, Height: 60},
		{Image: src, Width: 80, Height: 80, Metadata: "other"},
		{Image: mutatedPattern(160, 120), Width: 80, Height: 80}, // different pixels
	}
	for i, v := range variants {
		res, err := o.Crop(context.Background(), v)
		if err != nil {
			t.Fatalf("variant %d: %v", i, err)
		}
		if res.CacheHit {
			t.Fatalf("variant %d incorrectly hit the cache", i)
		}
	}
}

func TestOrchestratorDifferentConfigMissesCache(t *testing.T) {
	src := buildPattern(160, 120)
	a := mustOrchestrator(t, Config{Seed: 7})
	b := mustOrchestrator(t, Config{Seed: 8})
	req := Request{Image: src, Width: 80, Height: 80}
	if _, err := a.Crop(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res, err := b.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheHit {
		t.Fatal("different seeds share a cache key")
	}
}

func TestOrchestratorCacheIsolatedBetweenInstances(t *testing.T) {
	src := buildPattern(160, 120)
	a := mustOrchestrator(t, Config{Seed: 1})
	b := mustOrchestrator(t, Config{Seed: 1})
	req := Request{Image: src, Width: 80, Height: 80}
	if _, err := a.Crop(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res, err := b.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheHit {
		t.Fatal("cache leaked between orchestrators")
	}
	if a.CacheLen() != 1 || b.CacheLen() != 1 {
		t.Fatalf("cache lengths: %d and %d", a.CacheLen(), b.CacheLen())
	}
}

func TestOrchestratorCacheBounded(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3, CacheSize: 2})
	src := buildPattern(120, 120)
	// Four distinct aspect ratios: the fitted target aspect is part of the key.
	for _, dims := range [][2]int{{40, 40}, {50, 30}, {30, 50}, {70, 20}} {
		if _, err := o.Crop(context.Background(), Request{Image: src, Width: dims[0], Height: dims[1]}); err != nil {
			t.Fatal(err)
		}
	}
	if got := o.CacheLen(); got != 2 {
		t.Fatalf("cache holds %d entries, want 2", got)
	}
	if stats := o.CacheStats(); stats.CacheEvictions != 2 {
		t.Fatalf("evictions = %d, want 2", stats.CacheEvictions)
	}
}

func TestOrchestratorCacheDisabled(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3, CacheSize: -1})
	req := Request{Image: buildPattern(80, 80), Width: 40, Height: 40}
	if _, err := o.Crop(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	res, err := o.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.CacheHit || o.CacheLen() != 0 {
		t.Fatalf("disabled cache still stored entries")
	}
}

func TestOrchestratorCancelLeavesNoState(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := o.Crop(ctx, Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected zero result, got %+v", res)
	}
	if o.CacheLen() != 0 || o.ActiveWorkers() != 0 {
		t.Fatal("cancellation left state behind")
	}
}

func TestOrchestratorTimeoutLeavesNoState(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3, Timeout: time.Nanosecond})
	// Give the deadline a moment to expire before Crop runs.
	time.Sleep(2 * time.Millisecond)
	res, err := o.Crop(context.Background(), Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	if !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("got %v, want ErrDeadlineExceeded", err)
	}
	if !res.IsZero() {
		t.Fatalf("expected zero result, got %+v", res)
	}
	if o.CacheLen() != 0 || o.ActiveWorkers() != 0 {
		t.Fatal("timeout left state behind")
	}
}

func TestOrchestratorBudgetExhaustedLeavesNoState(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3, MaxCandidates: 2})
	res, err := o.Crop(context.Background(), Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v, want ErrBudgetExhausted", err)
	}
	if !res.IsZero() || o.CacheLen() != 0 {
		t.Fatal("budget exhaustion left state behind")
	}
}

func TestOrchestratorCandidateFailureLeavesNoState(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 3})
	o.scorer.failCandidate = func(Candidate) bool { return true }
	res, err := o.Crop(context.Background(), Request{Image: buildPattern(160, 120), Width: 80, Height: 60})
	if !errors.Is(err, ErrCandidateFailed) {
		t.Fatalf("got %v, want ErrCandidateFailed", err)
	}
	if !res.IsZero() || o.CacheLen() != 0 {
		t.Fatal("candidate failure left state behind")
	}
}

func TestOrchestratorValidationErrors(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 1})
	cases := []struct {
		name string
		req  Request
		want error
	}{
		{"nil image", Request{Width: 8, Height: 8}, ErrNilImage},
		{"zero dims", Request{Image: buildPattern(8, 8)}, ErrInvalidDimensions},
		{"negative", Request{Image: buildPattern(8, 8), Width: -4, Height: 8}, ErrInvalidDimensions},
		{"bad metadata", Request{Image: buildPattern(8, 8), Width: 4, Height: 4, Metadata: string([]byte{0x80})}, ErrInvalidMetadata},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := o.Crop(context.Background(), tc.req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if !res.IsZero() {
				t.Fatal("expected zero result")
			}
		})
	}
}

func TestOrchestratorConcurrentUse(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 6, Concurrency: 4})
	srcs := []image.Image{
		buildPattern(160, 120),
		buildAlphaEdges(),
		solid(color.RGBA{40, 80, 120, 255}, 128, 128),
		buildThin(32, 200),
	}
	const runs = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make(map[int]Result, runs)
	for i := 0; i < runs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := o.Crop(context.Background(), Request{Image: srcs[i%len(srcs)], Width: 64, Height: 64})
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				return
			}
			mu.Lock()
			results[i] = res
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if o.ActiveWorkers() != 0 {
		t.Fatal("workers still active after concurrent Crop calls")
	}
	if len(results) != runs {
		t.Fatalf("got %d of %d results", len(results), runs)
	}
	// Results for the same source must agree regardless of scheduling.
	for i := 0; i < runs; i++ {
		peer := results[i]
		other, ok := results[(i/len(srcs)+1)*len(srcs)+i%len(srcs)]
		if !ok {
			continue
		}
		if peer.Digest != other.Digest || peer.Rectangle != other.Rectangle {
			t.Fatalf("same source produced different results: %s vs %s", peer.Summary(), other.Summary())
		}
	}
}

func TestOrchestratorRepeatedSubmissionSameSummary(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 5, CacheSize: -1}) // no cache: purity check
	req := Request{Image: buildPattern(200, 160), Width: 100, Height: 80, Metadata: "重复提交"}
	var summaries []string
	for i := 0; i < 5; i++ {
		res, err := o.Crop(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		summaries = append(summaries, res.Summary())
	}
	for i := 1; i < len(summaries); i++ {
		if summaries[i] != summaries[0] {
			t.Fatalf("run %d summary %q != %q", i, summaries[i], summaries[0])
		}
	}
}

func TestOrchestratorSummaryFields(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 5})
	res, err := o.Crop(context.Background(), Request{Image: buildPattern(120, 90), Width: 60, Height: 60})
	if err != nil {
		t.Fatal(err)
	}
	if res.Digest == "" || res.ImageDigest == "" {
		t.Fatal("empty digests")
	}
	if res.TargetWidth <= 0 || res.TargetHeight <= 0 {
		t.Fatal("target dimensions not reported")
	}
	if len(res.Top) == 0 || len(res.Top) > o.Config().TopCandidates {
		t.Fatalf("top list length %d", len(res.Top))
	}
	if res.SourceRectangle != res.Rectangle {
		t.Fatalf("source rectangle %v should match canonical %v for zero-origin images", res.SourceRectangle, res.Rectangle)
	}
}

func TestOrchestratorNonZeroBoundsSourceRectangle(t *testing.T) {
	src := buildGrayscale(image.Rect(20, -15, 180, 105))
	o := mustOrchestrator(t, Config{Seed: 5})
	res, err := o.Crop(context.Background(), Request{Image: src, Width: 80, Height: 80})
	if err != nil {
		t.Fatal(err)
	}
	if res.SourceRectangle != res.Rectangle.Add(image.Pt(20, -15)) {
		t.Fatalf("source rectangle %v not translated from %v", res.SourceRectangle, res.Rectangle)
	}
	if !res.SourceRectangle.In(src.Bounds()) {
		t.Fatalf("source rectangle %v escapes source bounds %v", res.SourceRectangle, src.Bounds())
	}
}

func TestOrchestratorResultIndependentOfCacheMutation(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 5})
	req := Request{Image: buildPattern(160, 120), Width: 80, Height: 60}
	first, err := o.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	first.Top[0].Rectangle = image.Rect(0, 0, 1, 1) // mutate the returned copy
	second, err := o.Crop(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !second.CacheHit {
		t.Fatal("expected cache hit")
	}
	if second.Top[0].Rectangle == image.Rect(0, 0, 1, 1) {
		t.Fatal("caller mutation corrupted the cache")
	}
}

func TestOrchestratorStatsReported(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 5, Concurrency: 3})
	if _, err := o.Crop(context.Background(), Request{Image: buildPattern(160, 120), Width: 80, Height: 60}); err != nil {
		t.Fatal(err)
	}
	runs, fails := o.Runs()
	if runs != 1 || fails != 0 {
		t.Fatalf("runs=%d fails=%d", runs, fails)
	}
	stats := o.CacheStats()
	if stats.CacheMisses != 1 {
		t.Fatalf("misses = %d, want 1", stats.CacheMisses)
	}
}

func TestOrchestratorLargeBudgetImage(t *testing.T) {
	// 4 megapixels: large enough to exercise chunked normalization without
	// making the test slow.
	src := buildPattern(2048, 2048)
	o := mustOrchestrator(t, Config{Seed: 1, Concurrency: 4, MaxCandidates: 2000})
	res, err := o.Crop(context.Background(), Request{Image: src, Width: 1000, Height: 800})
	if err != nil {
		t.Fatalf("Crop: %v", err)
	}
	if res.Rectangle.Empty() || !res.Rectangle.In(image.Rect(0, 0, 2048, 2048)) {
		t.Fatalf("invalid rectangle %v", res.Rectangle)
	}
}

func TestOrchestratorPixelBudgetRejection(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 1})
	res, err := o.Crop(context.Background(), Request{Image: hugeBounds{rect: image.Rect(0, 0, 1<<20, 1<<20)}, Width: 10, Height: 10})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("got %v, want ErrBudgetExhausted", err)
	}
	if !res.IsZero() {
		t.Fatal("expected zero result")
	}

	tight := mustOrchestrator(t, Config{Seed: 1, PixelBudget: 100})
	if _, err := tight.Crop(context.Background(), Request{Image: buildPattern(16, 16), Width: 8, Height: 8}); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("tight pixel budget: got %v, want ErrBudgetExhausted", err)
	}
}

func TestOrchestratorMetadataBudget(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 1, MetadataBudget: 8})
	if _, err := o.Crop(context.Background(), Request{Image: buildPattern(16, 16), Width: 8, Height: 8, Metadata: "0123456789"}); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("got %v, want ErrInvalidMetadata", err)
	}
}

func TestNegativePixelOrMetadataBudgetConfig(t *testing.T) {
	for _, cfg := range []Config{{PixelBudget: -1}, {MetadataBudget: -1}} {
		if _, err := NewOrchestrator(cfg); !errors.Is(err, ErrNegativeBudget) {
			t.Fatalf("%+v: got %v, want ErrNegativeBudget", cfg, err)
		}
	}
}

func TestTopCandidatesCap(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 2, TopCandidates: 3})
	res, err := o.Crop(context.Background(), Request{Image: buildPattern(200, 200), Width: 100, Height: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Top) != 3 {
		t.Fatalf("top length = %d, want 3", len(res.Top))
	}
	if res.Stats.Scored < 3 {
		t.Fatalf("scored = %d", res.Stats.Scored)
	}
}

func TestSummaryStableAcrossRuns(t *testing.T) {
	cfg := Config{Seed: 13}
	req := Request{Image: buildPattern(180, 140), Width: 90, Height: 70, Metadata: "meta"}
	run := func() string {
		o := mustOrchestrator(t, cfg)
		res, err := o.Crop(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res.Summary()
	}
	if a, b := run(), run(); a != b {
		t.Fatalf("summaries differ: %q vs %q", a, b)
	}
}

func TestErrorIsMapping(t *testing.T) {
	_, err := NewOrchestrator(Config{Step: -1})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
	var opErr *OpError
	if !errors.As(err, &opErr) {
		t.Fatal("expected *OpError")
	}
	if opErr.Op == "" || opErr.Error() == "" {
		t.Fatal("OpError lacks diagnostics")
	}
	ctxErr := newOpError("x", ErrCanceled, "d")
	if !errors.Is(ctxErr, context.Canceled) || !errors.Is(ctxErr, ErrCanceled) {
		t.Fatal("canceled mapping broken")
	}
	deadlineErr := newOpError("x", ErrDeadlineExceeded, "")
	if !errors.Is(deadlineErr, context.DeadlineExceeded) || !errors.Is(deadlineErr, ErrDeadlineExceeded) {
		t.Fatal("deadline mapping broken")
	}
}

func TestConfigResolvedDefaults(t *testing.T) {
	cfg, err := Config{}.resolved()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Step != defaultStep || cfg.MinScale != defaultMinScale || cfg.MaxScale != defaultMaxScale ||
		cfg.MaxCandidates != defaultMaxCandidates || cfg.TopCandidates != defaultTopCandidates {
		t.Fatalf("unexpected defaults %+v", cfg)
	}
	if cfg.Concurrency < 1 {
		t.Fatalf("concurrency %d", cfg.Concurrency)
	}
}

func TestScoreFiniteForAllSamples(t *testing.T) {
	for _, s := range OfflineSamples() {
		o := mustOrchestrator(t, Config{Seed: 3})
		res, err := o.Crop(context.Background(), Request{Image: s.Image, Width: s.Width, Height: s.Height, Metadata: s.Metadata})
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		if math.IsNaN(res.TotalScore) || math.IsInf(res.TotalScore, 0) {
			t.Fatalf("%s: non-finite score %v", s.Name, res.TotalScore)
		}
	}
}

func TestDigestChangesWithPixels(t *testing.T) {
	a := buildPattern(64, 64)
	b := buildPattern(64, 64)
	b.SetRGBA(0, 0, color.RGBA{1, 2, 3, 255})
	na, err := Normalize(Request{Image: a, Width: 32, Height: 32}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	nb, err := Normalize(Request{Image: b, Width: 32, Height: 32}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if na.digest() == nb.digest() {
		t.Fatal("pixel change did not change the digest")
	}

	sorted := []string{"b", "a", "c"}
	sort.Strings(sorted)
	if hashStrings(sorted...) != hashStrings("a", "b", "c") {
		t.Fatal("hashStrings is order sensitive")
	}
	if hashStrings("a", "bc") == hashStrings("ab", "c") {
		t.Fatal("hashStrings is ambiguous about boundaries")
	}
}

func TestPublicEntryPoints(t *testing.T) {
	res, err := SmartCrop(context.Background(), Request{Image: buildPattern(120, 80), Width: 60, Height: 60})
	if err != nil {
		t.Fatalf("SmartCrop: %v", err)
	}
	if res.Rectangle.Empty() || res.Digest == "" {
		t.Fatalf("unexpected result %+v", res)
	}

	s := mustScorer(t, Config{Seed: 2, Concurrency: 2})
	if cfg := s.Config(); cfg.Seed != 2 || cfg.Concurrency != 2 {
		t.Fatalf("Config() = %+v", cfg)
	}
	n := mustNormalize(t, Request{Image: buildPattern(64, 48), Width: 32, Height: 32})
	if _, err := s.Rank(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if s.PeakWorkers() < 1 {
		t.Fatalf("PeakWorkers = %d", s.PeakWorkers())
	}
	if got := n.String(); got == "" || n.String() == "<nil>" {
		t.Fatalf("String() = %q", got)
	}
	var nilImage *NormalizedImage
	if got := nilImage.String(); got != "<nil>" {
		t.Fatalf("nil String() = %q", got)
	}

	// A nil orchestrator and a nil scorer must fail cleanly instead of panicking.
	var nilOrch *Orchestrator
	if _, err := nilOrch.Crop(context.Background(), Request{}); err == nil {
		t.Fatal("nil orchestrator returned no error")
	}
	if nilOrch.CacheLen() != 0 || nilOrch.ActiveWorkers() != 0 {
		t.Fatal("nil orchestrator accessors are not safe")
	}
	nilOrch.ClearCache()
	if stats := nilOrch.CacheStats(); stats != (Stats{}) {
		t.Fatalf("nil orchestrator stats = %+v", stats)
	}
	if runs, fails := nilOrch.Runs(); runs != 0 || fails != 0 {
		t.Fatal("nil orchestrator runs are not zero")
	}
}
