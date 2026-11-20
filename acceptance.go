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
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"sort"
	"strings"

	"github.com/muesli/smartcrop/nfnt" //nolint:gci // used by the pinned legacy check
)

// Sample is an offline acceptance sample. Images are generated from code so
// the acceptance run never depends on network access or fixture files.
type Sample struct {
	Name     string
	Image    image.Image
	Width    int
	Height   int
	Metadata string
}

// LegacyExpectation pins the legacy Analyzer result for an image. It is the
// regression anchor for "the old API keeps returning the same rectangle".
type LegacyExpectation struct {
	Image    image.Image
	Width    int
	Height   int
	Expected image.Rectangle
}

// AcceptanceOptions configures RunAcceptance.
type AcceptanceOptions struct {
	Config Config
	// Samples defaults to OfflineSamples().
	Samples []Sample
	// Legacy, when non-nil, pins the legacy Analyzer result.
	Legacy *LegacyExpectation
}

// CheckResult is the outcome of a single acceptance check.
type CheckResult struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// AcceptanceReport is the complete, comparable outcome of an acceptance run.
type AcceptanceReport struct {
	Checks  []CheckResult `json:"checks"`
	Failed  int           `json:"failed"`
	Passed  int           `json:"passed"`
	Digest  string        `json:"digest"`
	Summary string        `json:"summary"`
	// Limitations records conditions this environment could not cover. It is
	// part of the report so gaps are visible instead of silently skipped.
	Limitations []string `json:"limitations"`
}

// OK reports whether every check passed.
func (r AcceptanceReport) OK() bool {
	return r.Failed == 0 && len(r.Checks) > 0
}

// JSON returns an indented JSON rendering of the report.
func (r AcceptanceReport) JSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

// OfflineSamples returns deterministic samples covering grayscale, alpha,
// non-zero bounds, tiny images, solid colors, thin images and non-ASCII
// metadata. The same call always returns pixel-identical images.
func OfflineSamples() []Sample {
	return []Sample{
		{Name: "grayscale-nonzero-bounds", Image: buildGrayscale(image.Rect(11, -7, 171, 113)), Width: 100, Height: 50},
		{Name: "alpha-edges", Image: buildAlphaEdges(), Width: 120, Height: 120},
		{Name: "solid-color", Image: solid(color.RGBA{90, 90, 90, 255}, 160, 120), Width: 120, Height: 120},
		{Name: "thin-tall", Image: buildThin(48, 400), Width: 48, Height: 400},
		{Name: "non-ascii-metadata", Image: buildPattern(200, 140), Width: 160, Height: 100, Metadata: "猫咪-🐈 左上 / café"},
		{Name: "tiny", Image: buildPattern(9, 7), Width: 9, Height: 7},
		{Name: "large-budget", Image: buildPattern(768, 512), Width: 512, Height: 384},
	}
}

// RunAcceptance executes the acceptance suite against the offline samples. It
// returns a report even when checks fail; the caller decides how to surface it.
func RunAcceptance(ctx context.Context, opts AcceptanceOptions) AcceptanceReport {
	if ctx == nil {
		ctx = context.Background()
	}
	samples := opts.Samples
	if len(samples) == 0 {
		samples = OfflineSamples()
	}
	rep := AcceptanceReport{Limitations: defaultLimitations()}
	add := func(name string, err error, detail string) {
		cr := CheckResult{Name: name, Pass: err == nil, Detail: detail}
		if err != nil {
			cr.Detail = err.Error()
		}
		rep.Checks = append(rep.Checks, cr)
	}

	orchestrator, err := NewOrchestrator(opts.Config)
	if err != nil {
		add("config", err, "")
		return rep.finish()
	}

	for _, s := range samples {
		checkSample(ctx, orchestrator, s, add)
	}

	// Parallel scoring must reproduce the serial ranking exactly.
	add("parallel-equals-serial", checkParallelEqualsSerial(ctx, opts.Config, samples[0]), "")

	// Cancellation must leave no goroutines, cache entries or partial results.
	add("cancel-cleanup", checkCancelCleanup(ctx, opts.Config, samples[0]), "")

	// Timeout must surface ErrDeadlineExceeded and leave no cache entry.
	add("timeout", checkTimeout(ctx, opts.Config, samples[0]), "")

	// Corrupt sources must fail with a stable error and no result.
	add("corrupt-bounds", checkCorruptBounds(ctx, opts.Config), "")
	add("corrupt-pixels", checkCorruptPixels(ctx, opts.Config), "")
	add("nil-image", checkNilImage(ctx, opts.Config), "")

	// Budget exhaustion must fail atomically.
	add("budget-exhausted", checkBudgetExhausted(ctx, opts.Config, samples[0]), "")

	if opts.Legacy != nil {
		add("legacy-analyzer", checkLegacy(opts.Legacy), "")
	}

	return rep.finish()
}

func checkSample(ctx context.Context, o *Orchestrator, s Sample, add func(string, error, string)) {
	req := Request{Image: s.Image, Width: s.Width, Height: s.Height, Metadata: s.Metadata}
	first, err := o.Crop(ctx, req)
	add("sample:"+s.Name+":normalize", err, "")
	if err != nil {
		return
	}

	// Legality: non-empty, inside the canonical image and matching the target
	// aspect ratio to within one pixel of integer fitting.
	add("sample:"+s.Name+":legality", checkLegality(first, s), fmt.Sprintf("rect=%v", first.Rectangle))
	add("sample:"+s.Name+":ranking", checkRanking(first), fmt.Sprintf("top=%d", len(first.Top)))

	second, err := o.Crop(ctx, req)
	if err != nil {
		add("sample:"+s.Name+":digest-stability", err, "")
		return
	}
	var stability error
	if first.Digest != second.Digest || first.Rectangle != second.Rectangle || first.Summary() != second.Summary() {
		stability = fmt.Errorf("unstable result: %s vs %s", first.Summary(), second.Summary())
	}
	add("sample:"+s.Name+":digest-stability", stability, first.Summary())
}

func checkLegality(r Result, s Sample) error {
	if r.Rectangle.Empty() {
		return errors.New("empty rectangle")
	}
	b := s.Image.Bounds()
	canonical := image.Rect(0, 0, b.Dx(), b.Dy())
	if !r.Rectangle.In(canonical) {
		return fmt.Errorf("rectangle %v outside canonical %v", r.Rectangle, canonical)
	}
	if r.SourceRectangle != r.Rectangle.Add(b.Min) {
		return fmt.Errorf("source rectangle %v does not translate %v by %v", r.SourceRectangle, r.Rectangle, b.Min)
	}
	if r.TargetWidth < 1 || r.TargetHeight < 1 {
		return fmt.Errorf("invalid target %dx%d", r.TargetWidth, r.TargetHeight)
	}
	// Every candidate must keep the target aspect ratio within a pixel of
	// rounding and must be clipped to the image.
	if absInt(r.Rectangle.Dx()*r.TargetHeight-r.Rectangle.Dy()*r.TargetWidth) > maxInt(r.TargetWidth, r.TargetHeight) {
		return fmt.Errorf("aspect mismatch: %dx%d vs target %dx%d", r.Rectangle.Dx(), r.Rectangle.Dy(), r.TargetWidth, r.TargetHeight)
	}
	if r.TotalScore != r.TotalScore || math.IsInf(r.TotalScore, 0) {
		return fmt.Errorf("non-finite total score %v (%s)", r.TotalScore, classifyScore(r.TotalScore))
	}
	return nil
}

func checkRanking(r Result) error {
	if len(r.Top) == 0 {
		return errors.New("empty top list")
	}
	if r.Top[0].Rectangle != r.Rectangle {
		return fmt.Errorf("top[0]=%v differs from result %v", r.Top[0].Rectangle, r.Rectangle)
	}
	for i := 1; i < len(r.Top); i++ {
		if candidateLess(r.Top[i], r.Top[i-1]) {
			return fmt.Errorf("top[%d]=%v sorts before top[%d]=%v", i, r.Top[i].TotalScore, i-1, r.Top[i-1].TotalScore)
		}
	}
	if r.Stats.Scored != r.Stats.Candidates {
		return fmt.Errorf("scored %d of %d candidates", r.Stats.Scored, r.Stats.Candidates)
	}
	return nil
}

func checkParallelEqualsSerial(ctx context.Context, cfg Config, s Sample) error {
	serialCfg := cfg
	serialCfg.Concurrency = 1
	parallelCfg := cfg
	parallelCfg.Concurrency = 4

	serial, err := NewOrchestrator(serialCfg)
	if err != nil {
		return err
	}
	parallel, err := NewOrchestrator(parallelCfg)
	if err != nil {
		return err
	}
	req := Request{Image: s.Image, Width: s.Width, Height: s.Height, Metadata: s.Metadata}
	a, err := serial.Crop(ctx, req)
	if err != nil {
		return fmt.Errorf("serial run: %w", err)
	}
	b, err := parallel.Crop(ctx, req)
	if err != nil {
		return fmt.Errorf("parallel run: %w", err)
	}
	if a.Digest != b.Digest || a.Summary() != b.Summary() {
		return fmt.Errorf("serial %s != parallel %s", a.Summary(), b.Summary())
	}

	// Ranking itself must match candidate-for-candidate, not just the winner.
	na, err := Normalize(req, Limits{})
	if err != nil {
		return err
	}
	sa, err := NewScorer(serialCfg)
	if err != nil {
		return err
	}
	sb, err := NewScorer(parallelCfg)
	if err != nil {
		return err
	}
	ra, err := sa.Rank(ctx, na)
	if err != nil {
		return err
	}
	rb, err := sb.Rank(ctx, na)
	if err != nil {
		return err
	}
	if len(ra.Candidates) != len(rb.Candidates) {
		return fmt.Errorf("candidate count %d != %d", len(ra.Candidates), len(rb.Candidates))
	}
	for i := range ra.Candidates {
		if ra.Candidates[i] != rb.Candidates[i] {
			return fmt.Errorf("candidate %d differs: %+v vs %+v", i, ra.Candidates[i], rb.Candidates[i])
		}
	}
	return nil
}

func checkCancelCleanup(ctx context.Context, cfg Config, s Sample) error {
	cfg.CacheSize = 8
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	res, err := o.Crop(cancelCtx, Request{Image: s.Image, Width: s.Width, Height: s.Height})
	if err == nil {
		return errors.New("expected cancellation error")
	}
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		return fmt.Errorf("expected ErrCanceled/context.Canceled, got %v", err)
	}
	if !res.IsZero() {
		return fmt.Errorf("expected zero result on cancellation, got %+v", res)
	}
	if got := o.ActiveWorkers(); got != 0 {
		return fmt.Errorf("%d scoring workers still active", got)
	}
	if got := o.CacheLen(); got != 0 {
		return fmt.Errorf("%d cache entries after cancellation", got)
	}
	return nil
}

func checkTimeout(ctx context.Context, cfg Config, s Sample) error {
	cfg.Timeout = 1
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	res, err := o.Crop(ctx, Request{Image: s.Image, Width: s.Width, Height: s.Height})
	if err == nil {
		return errors.New("expected deadline error")
	}
	if !errors.Is(err, ErrDeadlineExceeded) || !errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("expected ErrDeadlineExceeded, got %v", err)
	}
	if !res.IsZero() {
		return errors.New("expected zero result on timeout")
	}
	if o.CacheLen() != 0 {
		return errors.New("timeout populated the cache")
	}
	return nil
}

// panickingBounds reports valid bounds but panics instead of returning them.
type panickingBounds struct{}

func (panickingBounds) ColorModel() color.Model { return color.RGBAModel }
func (panickingBounds) Bounds() image.Rectangle { panic("bounds are broken") }
func (panickingBounds) At(int, int) color.Color { return color.RGBA{} }

// panickingPixels reports valid bounds but panics on the first pixel read.
type panickingPixels struct{}

func (panickingPixels) ColorModel() color.Model { return color.RGBAModel }
func (panickingPixels) Bounds() image.Rectangle { return image.Rect(0, 0, 32, 32) }
func (panickingPixels) At(int, int) color.Color { panic("pixels are broken") }

func checkCorruptBounds(ctx context.Context, cfg Config) error {
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	res, err := o.Crop(ctx, Request{Image: panickingBounds{}, Width: 16, Height: 16})
	if !errors.Is(err, ErrUnreadablePixels) {
		return fmt.Errorf("expected ErrUnreadablePixels, got %v", err)
	}
	if !res.IsZero() {
		return errors.New("expected zero result")
	}
	return nil
}

func checkCorruptPixels(ctx context.Context, cfg Config) error {
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	res, err := o.Crop(ctx, Request{Image: panickingPixels{}, Width: 16, Height: 16})
	if !errors.Is(err, ErrUnreadablePixels) {
		return fmt.Errorf("expected ErrUnreadablePixels, got %v", err)
	}
	if !res.IsZero() {
		return errors.New("expected zero result")
	}
	return nil
}

func checkNilImage(ctx context.Context, cfg Config) error {
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	if _, err := o.Crop(ctx, Request{Width: 10, Height: 10}); !errors.Is(err, ErrNilImage) {
		return fmt.Errorf("expected ErrNilImage, got %v", err)
	}
	return nil
}

func checkBudgetExhausted(ctx context.Context, cfg Config, s Sample) error {
	cfg.MaxCandidates = 1
	cfg.CacheSize = 4
	o, err := NewOrchestrator(cfg)
	if err != nil {
		return err
	}
	res, err := o.Crop(ctx, Request{Image: s.Image, Width: s.Width, Height: s.Height})
	if !errors.Is(err, ErrBudgetExhausted) {
		return fmt.Errorf("expected ErrBudgetExhausted, got %v", err)
	}
	if !res.IsZero() {
		return errors.New("expected zero result")
	}
	if o.CacheLen() != 0 {
		return errors.New("budget exhaustion populated the cache")
	}
	return nil
}

func checkLegacy(legacy *LegacyExpectation) error {
	analyzer := NewAnalyzer(nfnt.NewDefaultResizer())
	rect, err := analyzer.FindBestCrop(legacy.Image, legacy.Width, legacy.Height)
	if err != nil {
		return err
	}
	if rect != legacy.Expected {
		return fmt.Errorf("legacy Analyzer returned %v, want %v", rect, legacy.Expected)
	}
	return nil
}

// finish finalizes counters, digest and summary.
func (r *AcceptanceReport) finish() AcceptanceReport {
	names := make([]string, 0, len(r.Checks))
	r.Passed, r.Failed = 0, 0
	for _, c := range r.Checks {
		if c.Pass {
			r.Passed++
		} else {
			r.Failed++
		}
		status := "FAIL"
		if c.Pass {
			status = "PASS"
		}
		names = append(names, c.Name+"="+status)
	}
	sort.Strings(names)
	r.Digest = hashStrings(names...)
	r.Summary = fmt.Sprintf("acceptance passed=%d failed=%d digest=%s", r.Passed, r.Failed, r.Digest)
	return *r
}

func defaultLimitations() []string {
	return []string{
		"EXIF/ICC metadata decoding is not exercised: no external image fixtures are downloaded.",
		"The legacy Analyzer comparison is limited to the single image pinned by LegacyExpectation.",
		"Timing-based checks assert cancellation semantics, not wall-clock performance.",
	}
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// buildPattern draws a deterministic, high-contrast pattern with a bright
// saliency blob off-center so crops are meaningful.
func buildPattern(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8((x*13 + y*7) % 256),
				G: uint8((x*3 + y*17) % 256),
				B: uint8((x*5 + y*11) % 256),
				A: 255,
			})
		}
	}
	blobW, blobH := w/4, h/4
	if blobW < 2 {
		blobW = 2
	}
	if blobH < 2 {
		blobH = 2
	}
	for y := h / 3; y < h/3+blobH && y < h; y++ {
		for x := w / 2; x < w/2+blobW && x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{245, 190, 150, 255})
		}
	}
	return img
}

// buildGrayscale returns a gray image with non-zero (and negative) bounds.
func buildGrayscale(r image.Rectangle) *image.Gray {
	img := image.NewGray(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((x*7 + y*3 + 256) % 256)})
		}
	}
	return img
}

// buildAlphaEdges returns an image with a fully transparent border. Alpha must
// be premultiplied during normalization and never crash scoring.
func buildAlphaEdges() *image.NRGBA {
	const w, h = 160, 120
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if x < 20 || y < 15 || x >= w-20 || y >= h-15 {
				img.SetNRGBA(x, y, color.NRGBA{})
				continue
			}
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 120, A: uint8(128 + (x+y)%128)})
		}
	}
	return img
}

// buildThin returns a very tall, narrow gradient image.
func buildThin(w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetGray(x, y, color.Gray{Y: uint8((y * 255) / h)})
		}
	}
	return img
}

func solid(c color.RGBA, w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// limitationSummary is used by the command to print gaps compactly.
func limitationSummary(limitations []string) string {
	return strings.Join(limitations, "; ")
}
