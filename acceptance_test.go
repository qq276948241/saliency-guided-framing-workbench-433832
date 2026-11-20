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
	"image"
	"os"
	"strings"
	"testing"

	"github.com/muesli/smartcrop/nfnt"
)

func TestRunAcceptanceAllPass(t *testing.T) {
	rep := RunAcceptance(context.Background(), AcceptanceOptions{Config: Config{Seed: 1}})
	for _, c := range rep.Checks {
		if !c.Pass {
			t.Errorf("check %q failed: %s", c.Name, c.Detail)
		}
	}
	if !rep.OK() {
		t.Fatalf("report not OK: %s", rep.Summary)
	}
	if rep.Passed == 0 || rep.Failed != 0 {
		t.Fatalf("passed=%d failed=%d", rep.Passed, rep.Failed)
	}
	if rep.Digest == "" {
		t.Fatal("empty report digest")
	}
	if len(rep.Limitations) == 0 {
		t.Fatal("report must record uncovered conditions")
	}
}

func TestRunAcceptanceRepeatable(t *testing.T) {
	cfg := Config{Seed: 21}
	a := RunAcceptance(context.Background(), AcceptanceOptions{Config: cfg})
	b := RunAcceptance(context.Background(), AcceptanceOptions{Config: cfg})
	if a.Digest != b.Digest || a.Summary != b.Summary {
		t.Fatalf("acceptance summaries differ: %s vs %s", a.Summary, b.Summary)
	}
	for i := range a.Checks {
		if a.Checks[i] != b.Checks[i] {
			t.Fatalf("check %d differs: %+v vs %+v", i, a.Checks[i], b.Checks[i])
		}
	}
}

func TestAcceptanceReportJSON(t *testing.T) {
	rep := RunAcceptance(context.Background(), AcceptanceOptions{Config: Config{Seed: 2}})
	data, err := rep.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var round AcceptanceReport
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if round.Summary != rep.Summary || len(round.Checks) != len(rep.Checks) {
		t.Fatal("JSON round trip lost data")
	}
}

func TestOfflineSamplesAreDeterministic(t *testing.T) {
	a := OfflineSamples()
	b := OfflineSamples()
	if len(a) != len(b) {
		t.Fatal("sample count changed")
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Metadata != b[i].Metadata {
			t.Fatalf("sample %d metadata differs", i)
		}
		na, err := Normalize(Request{Image: a[i].Image, Width: a[i].Width, Height: a[i].Height}, Limits{})
		if err != nil {
			t.Fatalf("%s: %v", a[i].Name, err)
		}
		nb, err := Normalize(Request{Image: b[i].Image, Width: b[i].Width, Height: b[i].Height}, Limits{})
		if err != nil {
			t.Fatalf("%s: %v", b[i].Name, err)
		}
		if na.digest() != nb.digest() {
			t.Fatalf("sample %q is not reproducible", a[i].Name)
		}
	}
}

func TestAcceptanceSampleCoverage(t *testing.T) {
	names := map[string]bool{}
	for _, s := range OfflineSamples() {
		names[s.Name] = true
	}
	for _, required := range []string{"grayscale-nonzero-bounds", "alpha-edges", "solid-color", "thin-tall", "non-ascii-metadata", "tiny", "large-budget"} {
		if !names[required] {
			t.Fatalf("missing required sample %q", required)
		}
	}
}

func TestAcceptanceFailsOnBrokenConfig(t *testing.T) {
	rep := RunAcceptance(context.Background(), AcceptanceOptions{Config: Config{Step: -3}})
	if rep.OK() {
		t.Fatal("expected failure report")
	}
	if rep.Failed != 1 || rep.Checks[0].Name != "config" {
		t.Fatalf("unexpected report %+v", rep)
	}
}

func TestAcceptanceLegacyPinned(t *testing.T) {
	// The example image ships with the repository; a missing fixture is a
	// hard failure rather than a skipped requirement.
	if _, err := os.Stat(testFile); err != nil {
		t.Fatalf("example image missing: %v", err)
	}
	fi, err := os.Open(testFile)
	if err != nil {
		t.Fatal(err)
	}
	defer fi.Close()
	img, _, err := image.Decode(fi)
	if err != nil {
		t.Fatal(err)
	}
	analyzer := NewAnalyzer(nfnt.NewDefaultResizer())
	rect, err := analyzer.FindBestCrop(img, 250, 250)
	if err != nil {
		t.Fatal(err)
	}
	rep := RunAcceptance(context.Background(), AcceptanceOptions{
		Config: Config{Seed: 1},
		Legacy: &LegacyExpectation{Image: img, Width: 250, Height: 250, Expected: rect},
	})
	if !rep.OK() {
		t.Fatalf("acceptance with legacy pin failed: %s", rep.Summary)
	}
}

func TestLegacyAnalyzerUnchanged(t *testing.T) {
	// The existing smartcrop_test.go pins the gopher result; this test pins
	// the failure mode and the deterministic internal helpers the new layer
	// now shares with the old one.
	analyzer := NewAnalyzer(nfnt.NewDefaultResizer())
	if _, err := analyzer.FindBestCrop(buildPattern(32, 32), 0, 0); !errors.Is(err, ErrInvalidDimensions) {
		t.Fatalf("got %v, want ErrInvalidDimensions", err)
	}

	src := buildPattern(200, 150)
	rect, err := analyzer.FindBestCrop(src, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	again, err := analyzer.FindBestCrop(src, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	if rect != again {
		t.Fatalf("legacy analyzer is not deterministic: %v vs %v", rect, again)
	}
	if !rect.In(src.Bounds()) || rect.Empty() {
		t.Fatalf("legacy analyzer returned illegal rectangle %v", rect)
	}
}

func TestLegacyHelpersMatchParameterizedKernel(t *testing.T) {
	src := buildPattern(64, 64)
	rgba := toRGBA(src)
	o := image.NewRGBA(rgba.Bounds())
	edgeDetect(rgba, o)
	skinDetect(rgba, o)
	saturationDetect(rgba, o)

	rect := image.Rect(8, 8, 40, 40)
	legacy := Crop{Rectangle: rect}
	want := score(o, legacy)
	got := scoreFor(o, Candidate{Rectangle: rect}, legacyScoringParams)
	if want != got {
		t.Fatalf("legacy score %+v != parameterized %+v", want, got)
	}
	for x := 0; x < 64; x += 7 {
		for y := 0; y < 64; y += 7 {
			if a, b := importance(legacy, x, y), importanceFor(rect, legacyScoringParams, x, y); a != b {
				t.Fatalf("importance differs at %d,%d: %v vs %v", x, y, a, b)
			}
		}
	}
}

func TestAcceptanceCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep := RunAcceptance(ctx, AcceptanceOptions{Config: Config{Seed: 1}})
	if rep.OK() {
		t.Fatal("expected failures for a canceled context")
	}
	if rep.Failed == 0 {
		t.Fatal("expected at least one failed check")
	}
}

func TestLimitationSummary(t *testing.T) {
	got := limitationSummary([]string{"a", "b"})
	if !strings.Contains(got, "a; b") {
		t.Fatalf("unexpected summary %q", got)
	}
}

func TestPanickingImagesAreStableAcrossEntryPoints(t *testing.T) {
	o := mustOrchestrator(t, Config{Seed: 1})
	for _, img := range []image.Image{panickingBounds{}, panickingPixels{}} {
		_, err := o.Crop(context.Background(), Request{Image: img, Width: 8, Height: 8})
		if !errors.Is(err, ErrUnreadablePixels) {
			t.Fatalf("%T: got %v, want ErrUnreadablePixels", img, err)
		}
	}
}
