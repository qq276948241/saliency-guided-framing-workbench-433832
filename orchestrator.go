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
	"fmt"
	"image"
	"math"
	"sync"
	"time"
)

// Result is the outcome of a successful orchestration run.
type Result struct {
	// Rectangle is the selected crop in canonical source coordinates, i.e.
	// relative to the normalized image with origin (0,0).
	Rectangle image.Rectangle
	// SourceRectangle is Rectangle translated back into the caller's original
	// coordinate system. For images with non-zero bounds this is the rectangle
	// to pass to SubImage.
	SourceRectangle image.Rectangle
	// Score is the raw saliency score of the selected rectangle.
	Score Score
	// TotalScore is the normalized score used for ranking.
	TotalScore float64
	// Scale is the candidate scale the winner was generated for.
	Scale float64
	// Digest identifies the exact (image pixels, request, algorithm version)
	// combination. Two runs with identical inputs always produce identical
	// digests; any input change produces a different digest.
	Digest string
	// ImageDigest fingerprints the canonical image pixels only.
	ImageDigest string
	// TargetWidth and TargetHeight are the fitted crop box dimensions.
	TargetWidth, TargetHeight int
	// Top holds the highest ranked candidates, capped by Config.TopCandidates.
	Top []Candidate
	// Stats is the observable run statistics, including cache counters.
	Stats Stats
	// CacheHit reports whether the result came from the cache.
	CacheHit bool
}

// Orchestrator ties normalization, candidate scoring and caching together.
//
// It is safe for concurrent use by multiple goroutines. All methods that
// mutate state (caching) are internally synchronized; requests never share
// mutable image data.
type Orchestrator struct {
	cfg    Config
	scorer *Scorer
	cache  *resultCache

	mu    sync.Mutex
	runs  int
	fails int
}

// NewOrchestrator validates cfg and returns an Orchestrator.
func NewOrchestrator(cfg Config) (*Orchestrator, error) {
	cfg, err := cfg.resolved()
	if err != nil {
		return nil, err
	}
	scorer, err := NewScorer(cfg)
	if err != nil {
		return nil, err
	}
	capacity := cfg.CacheSize
	if capacity < 0 {
		capacity = 0
	}
	return &Orchestrator{cfg: cfg, scorer: scorer, cache: newResultCache(capacity)}, nil
}

// Config returns the resolved configuration.
func (o *Orchestrator) Config() Config {
	return o.cfg
}

// Crop normalizes the request, scores candidates and returns the best crop.
//
// On success the result contains only confirmed data. On any error - invalid
// input, cancellation, timeout, exhausted budget or a failed candidate - the
// zero Result is returned, so callers can never observe a half-built crop.
// Failed and canceled runs never touch the cache.
func (o *Orchestrator) Crop(ctx context.Context, req Request) (Result, error) {
	if o == nil {
		return Result{}, newOpError("crop", ErrInvalidConfig, "nil orchestrator")
	}
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx, "crop"); err != nil {
		return Result{}, err
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if o.cfg.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, o.cfg.Timeout)
		defer cancel()
	}

	o.mu.Lock()
	o.runs++
	o.mu.Unlock()

	result, err := o.run(runCtx, req, start)
	if err != nil {
		o.mu.Lock()
		o.fails++
		o.mu.Unlock()
		return Result{}, err
	}
	return result, nil
}

func (o *Orchestrator) run(ctx context.Context, req Request, start time.Time) (Result, error) {
	n, err := normalizeContext(ctx, req, Limits{
		MaxPixels:        o.cfg.PixelBudget,
		MaxMetadataBytes: o.cfg.MetadataBudget,
	})
	if err != nil {
		return Result{}, err
	}

	imageDigest := n.digest()
	cacheKey := o.cacheKey(n, imageDigest)

	hits0, misses0, _, evictions0 := o.cache.snapshot()
	if cached, ok := o.cache.get(cacheKey); ok {
		hits1, misses1, entries1, evictions1 := o.cache.snapshot()
		cached.CacheHit = true
		cached.Stats.CacheHits = hits1 - hits0
		cached.Stats.CacheMisses = misses1 - misses0
		cached.Stats.CacheEntries = entries1
		cached.Stats.CacheEvictions = evictions1 - evictions0
		cached.Stats.Duration = time.Since(start)
		return cached, nil
	}

	ranking, err := o.scorer.Rank(ctx, n)
	if err != nil {
		return Result{}, err
	}
	if len(ranking.Candidates) == 0 {
		return Result{}, newOpError("crop", ErrNoCandidates, "")
	}

	best := ranking.Best
	top := ranking.Candidates
	if o.cfg.TopCandidates > 0 && len(top) > o.cfg.TopCandidates {
		top = top[:o.cfg.TopCandidates]
	}

	digest := hashStrings(algorithmVersion, o.cfg.fingerprint(), imageDigest,
		fmt.Sprintf("%dx%d", n.Width, n.Height),
		fmt.Sprintf("%dx%d", n.TargetWidth, n.TargetHeight),
		fmt.Sprintf("%d,%d,%d,%d", best.Min.X, best.Min.Y, best.Max.X, best.Max.Y),
		fmt.Sprintf("%016x", math.Float64bits(best.TotalScore)),
		req.Metadata)

	res := Result{
		Rectangle:       best.Rectangle,
		SourceRectangle: best.Rectangle.Add(n.originPoint()),
		TargetWidth:     n.TargetWidth,
		TargetHeight:    n.TargetHeight,
		Score:           best.Score,
		TotalScore:      best.TotalScore,
		Scale:           best.Scale,
		Digest:          digest,
		ImageDigest:     imageDigest,
		Top:             append([]Candidate(nil), top...),
		Stats:           ranking.Stats,
	}

	o.cache.put(cacheKey, res)
	hits1, misses1, entries1, evictions1 := o.cache.snapshot()
	res.Stats.CacheHits = hits1 - hits0
	res.Stats.CacheMisses = misses1 - misses0
	res.Stats.CacheEntries = entries1
	res.Stats.CacheEvictions = evictions1 - evictions0
	res.Stats.Duration = time.Since(start)
	return res, nil
}

// cacheKey folds the canonical pixel digest, the request shape and the config
// fingerprint. Concurrency, timeout and cache size are intentionally excluded
// so they never change the key.
func (o *Orchestrator) cacheKey(n *NormalizedImage, imageDigest string) string {
	return hashStrings(algorithmVersion, o.cfg.fingerprint(), imageDigest,
		fmt.Sprintf("%dx%d", n.Width, n.Height),
		fmt.Sprintf("%dx%d", n.TargetWidth, n.TargetHeight),
		n.Metadata)
}

// ClearCache drops every cached result. It is safe to call concurrently with
// Crop.
func (o *Orchestrator) ClearCache() {
	if o == nil {
		return
	}
	o.cache.clear()
}

// CacheLen reports the number of cached results.
func (o *Orchestrator) CacheLen() int {
	if o == nil {
		return 0
	}
	return o.cache.len()
}

// ActiveWorkers reports the number of scoring workers currently running. It is
// zero outside of a Crop call, which the acceptance tests assert after
// cancellation.
func (o *Orchestrator) ActiveWorkers() int {
	if o == nil || o.scorer == nil {
		return 0
	}
	return o.scorer.ActiveWorkers()
}

// CacheStats returns the cumulative cache counters.
func (o *Orchestrator) CacheStats() Stats {
	if o == nil {
		return Stats{}
	}
	hits, misses, entries, evictions := o.cache.snapshot()
	return Stats{CacheHits: hits, CacheMisses: misses, CacheEntries: entries, CacheEvictions: evictions}
}

// Runs reports how many Crop calls were started and how many failed.
func (o *Orchestrator) Runs() (runs, failures int) {
	if o == nil {
		return 0, 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.runs, o.fails
}

// IsZero reports whether r is the zero Result, i.e. the value returned for
// every failed, canceled or timed-out request. Because Result contains a slice
// it is not comparable with ==; use IsZero for the "no partial result"
// invariant.
func (r Result) IsZero() bool {
	return r.Rectangle.Empty() &&
		r.SourceRectangle.Empty() &&
		r.Digest == "" &&
		r.ImageDigest == "" &&
		len(r.Top) == 0 &&
		r.TotalScore == 0 &&
		r.Scale == 0 &&
		r.TargetWidth == 0 &&
		r.TargetHeight == 0
}

// SmartCrop is the one-shot, context-aware entry point for the orchestration
// layer. It is equivalent to creating an Orchestrator with the default Config
// (legacy weighting, rule of thirds, edge penalty, 8px grid) and calling Crop.
// The legacy Analyzer API is unchanged and keeps its own historical behavior.
func SmartCrop(ctx context.Context, req Request) (Result, error) {
	o, err := NewOrchestrator(Config{})
	if err != nil {
		return Result{}, err
	}
	return o.Crop(ctx, req)
}

// Summary returns a stable, human readable one-line summary of a result. Two
// runs over identical inputs must produce identical summaries; the acceptance
// command compares them byte for byte.
func (r Result) Summary() string {
	return fmt.Sprintf("digest=%s rect=%d,%d,%d,%d target=%dx%d score=%016x scale=%.1f top=%d",
		r.Digest,
		r.Rectangle.Min.X, r.Rectangle.Min.Y, r.Rectangle.Max.X, r.Rectangle.Max.Y,
		r.TargetWidth, r.TargetHeight,
		math.Float64bits(r.TotalScore), r.Scale,
		len(r.Top))
}
