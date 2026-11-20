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
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// algorithmVersion is folded into every cache key and result digest. Bump it
// whenever the scoring math or candidate generation changes.
const algorithmVersion = "smartcrop-orchestrator/v1"

const (
	defaultStep          = 8
	defaultMinScale      = 0.9
	defaultMaxScale      = 1.0
	defaultMaxCandidates = 100000
	defaultCacheSize     = 64
	defaultTopCandidates = 16
)

// Config controls deterministic candidate generation, scoring and caching.
//
// The zero value is usable and mirrors the legacy Analyzer grid: scales
// 1.0 and 0.9 in steps of 0.1, an 8px grid, rule of thirds enabled and the
// legacy edge penalty. The Seed only affects the optional deterministic
// jitter; with a fixed seed (including zero) the candidate list is identical
// on every run and every platform.
type Config struct {
	// Seed seeds the deterministic jitter generator. Concurrency and goroutine
	// scheduling never influence candidate positions.
	Seed uint64
	// DisableJitter turns off sub-grid jitter. Jitter is enabled by default.
	DisableJitter bool
	// Step is the candidate grid step in pixels. Zero selects 8.
	Step int
	// MinScale and MaxScale bound the candidate scale range. Zero values select
	// 0.9 and 1.0. Scales are enumerated in exact tenths from MaxScale down to
	// MinScale.
	MinScale, MaxScale float64
	// DisableRuleOfThirds turns off the rule-of-thirds boost in the importance
	// function. It is enabled by default, matching the legacy Analyzer.
	DisableRuleOfThirds bool
	// EdgeWeight overrides the legacy edge penalty (-20). A nil pointer keeps
	// the legacy value. The penalty applies to saliency pixels falling inside
	// the edge band of a crop.
	EdgeWeight *float64
	// MaxCandidates is the hard candidate budget. Zero selects 100000.
	// Exceeding it fails the whole request with ErrBudgetExhausted; no partial
	// ranking is returned.
	MaxCandidates int
	// PixelBudget caps the number of source pixels accepted per request. Zero
	// selects DefaultMaxPixels.
	PixelBudget int64
	// MetadataBudget caps the accepted metadata size in bytes. Zero selects
	// DefaultMaxMetadataBytes.
	MetadataBudget int
	// Concurrency is the number of scoring workers. Zero selects
	// runtime.GOMAXPROCS(0). Results do not depend on this value.
	Concurrency int
	// CacheSize bounds the number of cached results. Zero selects 64. Negative
	// values disable caching.
	CacheSize int
	// Timeout, when positive, bounds the whole request. It is combined with the
	// caller's context; whichever expires first wins.
	Timeout time.Duration
	// TopCandidates limits the ranked candidate list in Result.Top. Zero
	// selects 16. Ranking.Rank always contains every scored candidate.
	TopCandidates int
}

func (c Config) resolved() (Config, error) {
	if c.Step == 0 {
		c.Step = defaultStep
	}
	if c.Step < 1 {
		return c, newOpError("config", ErrInvalidConfig, wrapf("step must be positive, got %d", c.Step))
	}
	if c.MinScale == 0 {
		c.MinScale = defaultMinScale
	}
	if c.MaxScale == 0 {
		c.MaxScale = defaultMaxScale
	}
	if math.IsNaN(c.MinScale) || math.IsNaN(c.MaxScale) || math.IsInf(c.MinScale, 0) || math.IsInf(c.MaxScale, 0) {
		return c, newOpError("config", ErrInvalidConfig, wrapf("non-finite scale range %v..%v", c.MinScale, c.MaxScale))
	}
	if c.MinScale <= 0 || c.MaxScale < c.MinScale || c.MaxScale > 1 {
		return c, newOpError("config", ErrInvalidConfig, wrapf("scale range must satisfy 0 < min <= max <= 1, got %v..%v", c.MinScale, c.MaxScale))
	}
	if c.MaxCandidates == 0 {
		c.MaxCandidates = defaultMaxCandidates
	}
	if c.MaxCandidates < 0 {
		return c, newOpError("config", ErrNegativeBudget, wrapf("max candidates %d", c.MaxCandidates))
	}
	if c.PixelBudget < 0 {
		return c, newOpError("config", ErrNegativeBudget, wrapf("pixel budget %d", c.PixelBudget))
	}
	if c.PixelBudget == 0 {
		c.PixelBudget = DefaultMaxPixels
	}
	if c.MetadataBudget < 0 {
		return c, newOpError("config", ErrNegativeBudget, wrapf("metadata budget %d", c.MetadataBudget))
	}
	if c.MetadataBudget == 0 {
		c.MetadataBudget = DefaultMaxMetadataBytes
	}
	if c.Concurrency == 0 {
		c.Concurrency = runtime.GOMAXPROCS(0)
	}
	if c.Concurrency < 0 {
		return c, newOpError("config", ErrInvalidConfig, wrapf("negative concurrency %d", c.Concurrency))
	}
	if c.CacheSize == 0 {
		c.CacheSize = defaultCacheSize
	}
	if c.Timeout < 0 {
		return c, newOpError("config", ErrInvalidConfig, wrapf("negative timeout %v", c.Timeout))
	}
	if c.TopCandidates == 0 {
		c.TopCandidates = defaultTopCandidates
	}
	if c.TopCandidates < 0 {
		return c, newOpError("config", ErrInvalidConfig, wrapf("negative top candidate count %d", c.TopCandidates))
	}
	return c, nil
}

// fingerprint is the subset of the config that influences results. Cache size,
// concurrency and timeout are deliberately excluded: they must not change the
// selected rectangle.
func (c Config) fingerprint() string {
	edge := math.Float64bits(-20.0)
	if c.EdgeWeight != nil {
		edge = math.Float64bits(*c.EdgeWeight)
	}
	rot := "1"
	if c.DisableRuleOfThirds {
		rot = "0"
	}
	jitter := "1"
	if c.DisableJitter {
		jitter = "0"
	}
	return hashStrings(algorithmVersion,
		fmt.Sprintf("%d", c.Seed), jitter, fmt.Sprintf("%d", c.Step),
		fmt.Sprintf("%016x", math.Float64bits(c.MinScale)),
		fmt.Sprintf("%016x", math.Float64bits(c.MaxScale)),
		rot, fmt.Sprintf("%016x", edge),
		fmt.Sprintf("%d", c.MaxCandidates), fmt.Sprintf("%d", c.TopCandidates))
}

// Stats is an observable snapshot of a scoring or orchestration run.
type Stats struct {
	Candidates     int
	Scored         int
	Workers        int
	PeakWorkers    int
	ActiveWorkers  int
	CacheHits      int
	CacheMisses    int
	CacheEntries   int
	CacheEvictions int
	Duration       time.Duration
}

// Candidate is a scored crop rectangle.
type Candidate struct {
	// Rectangle is the candidate in canonical source coordinates, always fully
	// inside the source image and never empty.
	image.Rectangle
	// Scale is the scale factor the rectangle was generated for.
	Scale float64
	// Score holds the raw detail/skin/saturation sums.
	Score Score
	// TotalScore is the legacy normalized score:
	// (detail*0.2 + skin*1.8 + saturation*0.3) / area.
	TotalScore float64
	// Order is the deterministic generation index. It is the final tie-breaker
	// and never depends on goroutine scheduling.
	Order int
}

// Ranking is the full, deterministically ordered scoring result.
type Ranking struct {
	// Candidates is every scored candidate, best first. The ordering is
	// (TotalScore desc, Min.Y asc, Min.X asc, Max.X asc, Max.Y asc,
	// generation order asc).
	Candidates []Candidate
	// Best is Candidates[0].
	Best Candidate
	// Stats describes the run.
	Stats Stats
}

// Scorer generates and scores candidate rectangles for canonical images.
//
// Candidate generation is fully deterministic for a given image and Config:
//
//   - scales are enumerated in exact tenths from MaxScale down to MinScale;
//   - for every scale the candidate size is roundHalfUp(target*scale),
//     clamped to [1, source size];
//   - grid positions advance by Step; with jitter enabled, each grid point gets
//     a deterministic offset in [0, Step) from a PRNG seeded by Config.Seed and
//     the grid coordinates (not by iteration order);
//   - every rectangle is clipped to the image bounds and validated; empty or
//     duplicate rectangles are dropped.
//
// Scoring reuses the legacy importance function: an edge band of edgeRadius
// applies the edge penalty (EdgeWeight), and the rule-of-thirds term is applied
// unless DisableRuleOfThirds is set.
//
// Candidates are scored by independent workers. Workers only read the shared
// saliency map, which is immutable after construction, and each worker writes
// to its own candidate index, so the ranking is bit-for-bit identical to a
// single-worker (Concurrency=1) run.
type Scorer struct {
	cfg    Config
	params scoringParams

	active int64
	peak   int64
	// failCandidate, when non-nil, marks candidates whose scoring must fail.
	// It exists so tests can exercise the ErrCandidateFailed path without a
	// broken pixel reader; it is nil in production.
	failCandidate func(Candidate) bool
}

// NewScorer validates cfg and returns a Scorer.
func NewScorer(cfg Config) (*Scorer, error) {
	cfg, err := cfg.resolved()
	if err != nil {
		return nil, err
	}
	params := legacyScoringParams
	if cfg.DisableRuleOfThirds {
		params.ruleOfThirds = false
	}
	if cfg.EdgeWeight != nil {
		params.edgeWeight = *cfg.EdgeWeight
	}
	return &Scorer{cfg: cfg, params: params}, nil
}

// Config returns the resolved configuration.
func (s *Scorer) Config() Config {
	return s.cfg
}

// ActiveWorkers reports the number of scoring workers currently running. It is
// always zero once Rank has returned, including on cancellation and failure.
func (s *Scorer) ActiveWorkers() int {
	return int(atomic.LoadInt64(&s.active))
}

// PeakWorkers reports the high-water mark of concurrent scoring workers.
func (s *Scorer) PeakWorkers() int {
	return int(atomic.LoadInt64(&s.peak))
}

// workerStarted bumps the active worker counter and updates the lifetime peak
// with a CAS loop. It never blocks and never affects the result, only stats.
func (s *Scorer) workerStarted() {
	cur := atomic.AddInt64(&s.active, 1)
	bumpPeak(&s.peak, cur)
}

// bumpPeak raises *peak to at least cur.
func bumpPeak(peak *int64, cur int64) {
	for {
		old := atomic.LoadInt64(peak)
		if cur <= old || atomic.CompareAndSwapInt64(peak, old, cur) {
			return
		}
	}
}

// Rank generates, scores and orders every candidate for n.
//
// The returned error is one of ErrCanceled, ErrDeadlineExceeded,
// ErrBudgetExhausted, ErrNoCandidates, ErrInvalidRequest or
// ErrCandidateFailed. On error no partial Ranking is returned.
func (s *Scorer) Rank(ctx context.Context, n *NormalizedImage) (Ranking, error) {
	start := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx, "rank"); err != nil {
		return Ranking{}, err
	}
	if s == nil {
		return Ranking{}, newOpError("rank", ErrInvalidConfig, "nil scorer")
	}
	if n == nil || n.RGBA == nil {
		return Ranking{}, newOpError("rank", ErrInvalidRequest, "nil normalized image")
	}
	if n.Width <= 0 || n.Height <= 0 || n.RGBA.Bounds().Dx() != n.Width || n.RGBA.Bounds().Dy() != n.Height {
		return Ranking{}, newOpError("rank", ErrInvalidRequest, wrapf("normalized image %dx%d does not match RGBA bounds %v", n.Width, n.Height, n.RGBA.Bounds()))
	}

	candidates, err := s.generate(n)
	if err != nil {
		return Ranking{}, err
	}
	if len(candidates) == 0 {
		return Ranking{}, newOpError("rank", ErrNoCandidates, wrapf("target %dx%d in %dx%d", n.TargetWidth, n.TargetHeight, n.Width, n.Height))
	}

	if err := contextErr(ctx, "rank"); err != nil {
		return Ranking{}, err
	}
	saliency, err := buildSaliency(n.RGBA)
	if err != nil {
		return Ranking{}, err
	}
	if err := contextErr(ctx, "rank"); err != nil {
		return Ranking{}, err
	}

	scored, peak, err := s.scoreAll(ctx, saliency, candidates)
	if err != nil {
		return Ranking{}, err
	}

	sort.SliceStable(scored, func(i, j int) bool {
		return candidateLess(scored[i], scored[j])
	})

	stats := Stats{
		Candidates:    len(candidates),
		Scored:        len(scored),
		Workers:       s.workerCount(len(candidates)),
		PeakWorkers:   peak,
		ActiveWorkers: s.ActiveWorkers(),
		Duration:      time.Since(start),
	}
	return Ranking{Candidates: scored, Best: scored[0], Stats: stats}, nil
}

// ScoreCandidate scores a single rectangle against n. It is the public
// single-candidate entry point used for acceptance checks; an out-of-bounds,
// empty or otherwise illegal rectangle fails with ErrCandidateFailed and no
// Score is returned.
func (s *Scorer) ScoreCandidate(ctx context.Context, n *NormalizedImage, rect image.Rectangle) (Candidate, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextErr(ctx, "score-candidate"); err != nil {
		return Candidate{}, err
	}
	if n == nil || n.RGBA == nil {
		return Candidate{}, newOpError("score-candidate", ErrInvalidRequest, "nil normalized image")
	}
	if err := validateRect(rect, n.Width, n.Height); err != nil {
		return Candidate{}, err
	}
	saliency, err := buildSaliency(n.RGBA)
	if err != nil {
		return Candidate{}, err
	}
	if err := contextErr(ctx, "score-candidate"); err != nil {
		return Candidate{}, err
	}
	c := Candidate{Rectangle: rect, Scale: 1}
	c.Score, err = safeScore(saliency, c, s.params)
	if err != nil {
		return Candidate{}, err
	}
	c.TotalScore = c.Score.totalScore(c.Rectangle)
	return c, nil
}

func validateRect(rect image.Rectangle, width, height int) error {
	if rect.Empty() || rect.Dx() <= 0 || rect.Dy() <= 0 {
		return newOpError("score-candidate", ErrCandidateFailed, wrapf("empty rectangle %v", rect))
	}
	if rect.Min.X < 0 || rect.Min.Y < 0 || rect.Max.X > width || rect.Max.Y > height {
		return newOpError("score-candidate", ErrCandidateFailed, wrapf("rectangle %v outside %dx%d", rect, width, height))
	}
	if rect.Max.X <= rect.Min.X || rect.Max.Y <= rect.Min.Y {
		return newOpError("score-candidate", ErrCandidateFailed, wrapf("degenerate rectangle %v", rect))
	}
	return nil
}

// generate enumerates the deterministic candidate set.
func (s *Scorer) generate(n *NormalizedImage) ([]Candidate, error) {
	scales := scaleSequence(s.cfg.MinScale, s.cfg.MaxScale)
	step := s.cfg.Step
	seen := make(map[image.Rectangle]struct{})
	out := make([]Candidate, 0, 64)

	for si, scale := range scales {
		w := roundHalfUp(float64(n.TargetWidth) * scale)
		h := roundHalfUp(float64(n.TargetHeight) * scale)
		w = clampInt(w, 1, n.Width)
		h = clampInt(h, 1, n.Height)
		maxX := n.Width - w
		maxY := n.Height - h
		if maxX < 0 || maxY < 0 {
			continue
		}
		for y := 0; y <= maxY; y += step {
			for x := 0; x <= maxX; x += step {
				nx, ny := x, y
				if !s.cfg.DisableJitter {
					gx, gy := x/step, y/step
					seed := deriveSeed(s.cfg.Seed, si, gx, gy)
					gen := newSplitmix64(seed)
					nx = x + gen.nextInt(step)
					ny = y + gen.nextInt(step)
				}
				nx = clampInt(nx, 0, maxX)
				ny = clampInt(ny, 0, maxY)

				rect := image.Rect(nx, ny, nx+w, ny+h)
				// Explicit clipping: this is a no-op for well-formed
				// candidates, but guarantees rectangles can never leak outside
				// the image even if the arithmetic above changes.
				rect = rect.Intersect(image.Rect(0, 0, n.Width, n.Height))
				if rect.Empty() || rect.Dx() != w || rect.Dy() != h {
					continue
				}
				if _, ok := seen[rect]; ok {
					continue
				}
				if len(out) >= s.cfg.MaxCandidates {
					return nil, newOpError("generate", ErrBudgetExhausted,
						wrapf("more than %d candidates for %dx%d target %dx%d", s.cfg.MaxCandidates, n.Width, n.Height, n.TargetWidth, n.TargetHeight))
				}
				seen[rect] = struct{}{}
				out = append(out, Candidate{Rectangle: rect, Scale: scale, Order: len(out)})
			}
		}
	}
	return out, nil
}

// scaleSequence returns exact tenths from max down to min, inclusive.
func scaleSequence(minScale, maxScale float64) []float64 {
	start := int(math.Floor(maxScale*10 + 1e-9))
	end := int(math.Ceil(minScale*10 - 1e-9))
	if start < end {
		return nil
	}
	out := make([]float64, 0, start-end+1)
	for tenths := start; tenths >= end; tenths-- {
		out = append(out, float64(tenths)/10.0)
	}
	return out
}

func (s *Scorer) workerCount(n int) int {
	workers := s.cfg.Concurrency
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}
	return workers
}

// scoreAll scores candidates with a bounded worker pool. Each worker writes
// distinct indices of results, so no lock is needed and the outcome is
// independent of scheduling. The shared saliency image is treated as
// read-only.
func (s *Scorer) scoreAll(ctx context.Context, saliency *image.RGBA, candidates []Candidate) ([]Candidate, int, error) {
	results := make([]Candidate, len(candidates))
	workers := s.workerCount(len(candidates))
	var next int64 = -1
	var failed int64
	var runActive, runPeak int64
	var wg sync.WaitGroup
	wg.Add(workers)

	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			s.workerStarted()
			defer atomic.AddInt64(&s.active, -1)
			cur := atomic.AddInt64(&runActive, 1)
			bumpPeak(&runPeak, cur)
			defer atomic.AddInt64(&runActive, -1)

			for {
				if ctx.Err() != nil {
					return
				}
				if atomic.LoadInt64(&failed) != 0 {
					return
				}
				i := atomic.AddInt64(&next, 1)
				if i >= int64(len(candidates)) {
					return
				}
				c := candidates[i]
				if s.failCandidate != nil && s.failCandidate(c) {
					atomic.StoreInt64(&failed, 1)
					return
				}
				sc, err := safeScore(saliency, c, s.params)
				if err != nil {
					atomic.StoreInt64(&failed, 1)
					return
				}
				c.Score = sc
				c.TotalScore = sc.totalScore(c.Rectangle)
				results[i] = c
			}
		}()
	}
	wg.Wait()

	if err := contextErr(ctx, "score"); err != nil {
		return nil, 0, err
	}
	if atomic.LoadInt64(&failed) != 0 {
		return nil, 0, newOpError("score", ErrCandidateFailed, "a candidate could not be scored")
	}
	for i := range results {
		if results[i].Rectangle.Empty() {
			return nil, 0, newOpError("score", ErrCandidateFailed, wrapf("candidate %d missing after scoring", i))
		}
	}
	return results, int(runPeak), nil
}

// safeScore isolates panics in scoring so a broken pixel reader becomes a
// stable ErrCandidateFailed instead of crashing the process.
func safeScore(saliency *image.RGBA, c Candidate, p scoringParams) (sc Score, err error) {
	defer func() {
		if r := recover(); r != nil {
			sc = Score{}
			err = newOpError("score", ErrCandidateFailed, wrapf("panic scoring %v: %v", c.Rectangle, r))
		}
	}()
	return scoreFor(saliency, c, p), nil
}

// candidateLess defines the total order used for ranking. It is stable and
// total: no two distinct candidate indices compare equal. Non-finite scores
// (which scoring cannot produce, but which tests may inject) rank last so a
// NaN can never become the winner through an inconsistent comparison.
func candidateLess(a, b Candidate) bool {
	as, bs := a.TotalScore, b.TotalScore
	if as != bs {
		aBad := math.IsNaN(as) || math.IsInf(as, 0)
		bBad := math.IsNaN(bs) || math.IsInf(bs, 0)
		if aBad != bBad {
			return !aBad
		}
		if !aBad {
			return as > bs
		}
	}
	if a.Min.Y != b.Min.Y {
		return a.Min.Y < b.Min.Y
	}
	if a.Min.X != b.Min.X {
		return a.Min.X < b.Min.X
	}
	if a.Max.X != b.Max.X {
		return a.Max.X < b.Max.X
	}
	if a.Max.Y != b.Max.Y {
		return a.Max.Y < b.Max.Y
	}
	return a.Order < b.Order
}

// classifyScore identifies non-finite totals so tests and acceptance reports
// can diagnose them instead of treating NaN as a valid winner.
func classifyScore(v float64) string {
	switch {
	case math.IsNaN(v):
		return "nan"
	case math.IsInf(v, 1):
		return "+inf"
	case math.IsInf(v, -1):
		return "-inf"
	default:
		return "finite"
	}
}

// deriveSeed mixes the seed with integer coordinates. It is deterministic and
// independent of evaluation order.
func deriveSeed(seed uint64, parts ...int) uint64 {
	s := newSplitmix64(seed ^ 0xA0761D6478BD642F)
	for _, p := range parts {
		s.state ^= uint64(int64(p)) * 0x9E3779B97F4A7C15
		_ = s.next()
	}
	return s.next()
}
