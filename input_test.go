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
	"strings"
	"testing"
)

func TestNormalizeGrayscaleNonZeroBounds(t *testing.T) {
	src := buildGrayscale(image.Rect(-9, 4, 51, 44))
	n, err := Normalize(Request{Image: src, Width: 30, Height: 20}, Limits{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if n.Width != 60 || n.Height != 40 {
		t.Fatalf("canonical size = %dx%d, want 60x40", n.Width, n.Height)
	}
	if n.Origin != image.Pt(-9, 4) {
		t.Fatalf("origin = %v, want (-9,4)", n.Origin)
	}
	if got := n.RGBA.Bounds(); got != image.Rect(0, 0, 60, 40) {
		t.Fatalf("RGBA bounds = %v, want canonical origin", got)
	}
	// Canonical pixel (0,0) must correspond to source pixel (-9,4).
	want := src.GrayAt(-9, 4).Y
	if got := n.RGBA.RGBAAt(0, 0).R; got != want {
		t.Fatalf("canonical pixel = %d, want %d", got, want)
	}
	if n.TargetWidth != 60 || n.TargetHeight != 40 {
		t.Fatalf("target = %dx%d, want source aspect 60x40", n.TargetWidth, n.TargetHeight)
	}
}

func TestNormalizeDoesNotAliasSource(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 32, 32))
	src.SetRGBA(1, 1, color.RGBA{10, 20, 30, 255})
	n, err := Normalize(Request{Image: src, Width: 16, Height: 16}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	before := n.RGBA.RGBAAt(1, 1)
	src.SetRGBA(1, 1, color.RGBA{200, 200, 200, 255})
	if after := n.RGBA.RGBAAt(1, 1); after != before {
		t.Fatalf("normalized image aliases the source: %v -> %v", before, after)
	}
}

func TestNormalizePremultipliesAlpha(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	src.SetNRGBA(3, 3, color.NRGBA{R: 200, G: 100, B: 50, A: 128})
	n, err := Normalize(Request{Image: src, Width: 8, Height: 8}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	got := n.RGBA.RGBAAt(3, 3)
	if got.A != 128 {
		t.Fatalf("alpha = %d, want 128", got.A)
	}
	if got.R >= 200 {
		t.Fatalf("expected premultiplied red below 200, got %d", got.R)
	}
}

func TestNormalizeTinyImage(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 1, 1))
	n, err := Normalize(Request{Image: src, Width: 400, Height: 400}, Limits{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if n.TargetWidth != 1 || n.TargetHeight != 1 {
		t.Fatalf("target = %dx%d, want 1x1", n.TargetWidth, n.TargetHeight)
	}
}

func TestNormalizeTargetAspect(t *testing.T) {
	src := buildPattern(400, 300)
	cases := []struct {
		w, h  int
		wantW int
		wantH int
	}{
		{100, 50, 400, 200},
		{100, 100, 300, 300},
		{0, 50, 400, 50}, // zero width -> source width
		{50, 0, 50, 300}, // zero height -> source height
	}
	for _, tc := range cases {
		n, err := Normalize(Request{Image: src, Width: tc.w, Height: tc.h}, Limits{})
		if err != nil {
			t.Fatalf("%dx%d: %v", tc.w, tc.h, err)
		}
		if n.TargetWidth != tc.wantW || n.TargetHeight != tc.wantH {
			t.Fatalf("%dx%d: target = %dx%d, want %dx%d", tc.w, tc.h, n.TargetWidth, n.TargetHeight, tc.wantW, tc.wantH)
		}
	}
}

func TestNormalizeRejects(t *testing.T) {
	src := buildPattern(16, 16)
	cases := []struct {
		name   string
		req    Request
		limits Limits
		want   error
	}{
		{"nil image", Request{Width: 8, Height: 8}, Limits{}, ErrNilImage},
		{"zero dims", Request{Image: src}, Limits{}, ErrInvalidDimensions},
		{"negative dims", Request{Image: src, Width: -1, Height: 8}, Limits{}, ErrInvalidDimensions},
		{"oversized request", Request{Image: src, Width: int(maxTargetDimension) + 1, Height: 8}, Limits{}, ErrDimensionOverflow},
		{"negative pixel budget", Request{Image: src, Width: 8, Height: 8}, Limits{MaxPixels: -1}, ErrNegativeBudget},
		{"negative metadata budget", Request{Image: src, Width: 8, Height: 8}, Limits{MaxMetadataBytes: -1}, ErrNegativeBudget},
		{"metadata too large", Request{Image: src, Width: 8, Height: 8, Metadata: strings.Repeat("x", 10)}, Limits{MaxMetadataBytes: 4}, ErrInvalidMetadata},
		{"metadata invalid utf8", Request{Image: src, Width: 8, Height: 8, Metadata: string([]byte{0xff, 0xfe})}, Limits{}, ErrInvalidMetadata},
		{"pixel budget exceeded", Request{Image: src, Width: 8, Height: 8}, Limits{MaxPixels: 10}, ErrBudgetExhausted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Normalize(tc.req, tc.limits); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

// hugeBounds reports bounds whose pixel count overflows without allocating.
type hugeBounds struct{ rect image.Rectangle }

func (h hugeBounds) ColorModel() color.Model { return color.RGBAModel }
func (h hugeBounds) Bounds() image.Rectangle { return h.rect }
func (h hugeBounds) At(int, int) color.Color { return color.RGBA{} }

func TestNormalizeRejectsOverflowingBounds(t *testing.T) {
	src := hugeBounds{rect: image.Rect(0, 0, 1<<40, 1<<40)}
	if _, err := Normalize(Request{Image: src, Width: 8, Height: 8}, Limits{MaxPixels: 1 << 50}); !errors.Is(err, ErrDimensionOverflow) {
		t.Fatalf("got %v, want ErrDimensionOverflow", err)
	}
	// Even under a permissive pixel budget the product must not overflow.
	src = hugeBounds{rect: image.Rect(-(1 << 34), 0, 1<<34, 1<<34)}
	if _, err := Normalize(Request{Image: src, Width: 8, Height: 8}, Limits{MaxPixels: 1 << 62}); !errors.Is(err, ErrDimensionOverflow) {
		t.Fatalf("got %v, want ErrDimensionOverflow", err)
	}
}

func TestNormalizeRejectsEmptyBounds(t *testing.T) {
	src := image.NewRGBA(image.Rect(5, 5, 5, 9))
	if _, err := Normalize(Request{Image: src, Width: 8, Height: 8}, Limits{}); !errors.Is(err, ErrEmptyImage) {
		t.Fatalf("got %v, want ErrEmptyImage", err)
	}
}

func TestNormalizeReadsEveryPixelFamily(t *testing.T) {
	srcs := []image.Image{
		image.NewGray(image.Rect(3, 3, 19, 17)),
		image.NewNRGBA(image.Rect(0, 0, 16, 14)),
		image.NewRGBA(image.Rect(-4, -4, 12, 10)),
		image.NewPaletted(image.Rect(0, 0, 16, 16), color.Palette{color.Black, color.White}),
	}
	for i, src := range srcs {
		if _, err := Normalize(Request{Image: src, Width: 8, Height: 8}, Limits{}); err != nil {
			t.Fatalf("source %d (%T): %v", i, src, err)
		}
	}
}

func TestNormalizeAcceptsSubImage(t *testing.T) {
	base := buildPattern(64, 64)
	sub := base.SubImage(image.Rect(16, 8, 48, 40))
	n, err := Normalize(Request{Image: sub, Width: 16, Height: 16}, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if n.Width != 32 || n.Height != 32 || n.Origin != image.Pt(16, 8) {
		t.Fatalf("sub-image normalized to %v origin %v", n, n.Origin)
	}
}

func TestNormalizeUnreadablePixels(t *testing.T) {
	if _, err := Normalize(Request{Image: panickingPixels{}, Width: 8, Height: 8}, Limits{}); !errors.Is(err, ErrUnreadablePixels) {
		t.Fatalf("got %v, want ErrUnreadablePixels", err)
	}
	if _, err := Normalize(Request{Image: panickingBounds{}, Width: 8, Height: 8}, Limits{}); !errors.Is(err, ErrUnreadablePixels) {
		t.Fatalf("got %v, want ErrUnreadablePixels", err)
	}
}

func TestNormalizeCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := normalizeContext(ctx, Request{Image: buildPattern(32, 32), Width: 16, Height: 16}, Limits{}); !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
}

// slowImage blocks on every pixel read so cancellation during normalization
// can be exercised deterministically.
type slowImage struct {
	image.Image
	delay  chan struct{}
	perRow int
}

func (s slowImage) At(x, y int) color.Color {
	if s.perRow == 0 {
		<-s.delay
	}
	return s.Image.At(x, y)
}

func TestNormalizeCancellationDuringCopy(t *testing.T) {
	release := make(chan struct{})
	src := slowImage{Image: buildPattern(16, 600), delay: release}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		cancel()
		close(release)
	}()
	_, err := normalizeContext(ctx, Request{Image: src, Width: 16, Height: 16}, Limits{})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("got %v, want ErrCanceled", err)
	}
}
