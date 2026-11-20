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
	"image/draw"
	"math"
	"unicode/utf8"
)

// Limits bounds the work the normalization step is willing to do.
type Limits struct {
	// MaxPixels is the maximum number of source pixels Normalize will accept.
	// Zero selects DefaultMaxPixels.
	MaxPixels int64
	// MaxMetadataBytes is the maximum accepted metadata size. Zero selects
	// DefaultMaxMetadataBytes.
	MaxMetadataBytes int
}

const (
	// DefaultMaxPixels is the default pixel budget: 32 megapixels.
	DefaultMaxPixels int64 = 32 << 20
	// DefaultMaxMetadataBytes is the default metadata budget: 64 KiB.
	DefaultMaxMetadataBytes = 64 << 10
	// maxTargetDimension rejects absurd requested crop boxes before any float
	// arithmetic can lose precision.
	maxTargetDimension = int64(1) << 30
)

// Request describes a single saliency crop request.
type Request struct {
	// Image is the source image. It is never mutated or aliased by the
	// orchestrator.
	Image image.Image
	// Width and Height describe the requested crop box. At most one of them may
	// be zero; a zero dimension falls back to the source dimension. The actual
	// candidate crop is the largest box with the requested aspect ratio that
	// fits the source image, which matches the behavior of Analyzer.
	Width, Height int
	// Metadata is an optional, caller supplied label. It must be valid UTF-8
	// and is part of the result digest and the cache key. It is never
	// interpreted.
	Metadata string
}

// NormalizedImage is an owned, canonical copy of the request image.
//
// The RGBA field always has origin (0,0), is never aliased with the caller's
// image, and is immutable after Normalize returns. All scoring goroutines may
// read it concurrently.
type NormalizedImage struct {
	RGBA *image.RGBA
	// Width and Height are the canonical pixel dimensions.
	Width, Height int
	// TargetWidth and TargetHeight are the largest crop box with the requested
	// aspect ratio that fits the image. They are always >= 1 and <= Width/Height.
	TargetWidth, TargetHeight int
	// Aspect is TargetWidth/TargetHeight after integer fitting.
	Aspect float64
	// Origin is the source image's bounds origin. Canonical coordinates are
	// relative to it.
	Origin image.Point
	// Metadata is the validated request metadata.
	Metadata string
}

func (l Limits) resolve() (Limits, error) {
	if l.MaxPixels < 0 || l.MaxMetadataBytes < 0 {
		return l, newOpError("normalize", ErrNegativeBudget, wrapf("maxPixels=%d maxMetadataBytes=%d", l.MaxPixels, l.MaxMetadataBytes))
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = DefaultMaxPixels
	}
	if l.MaxMetadataBytes == 0 {
		l.MaxMetadataBytes = DefaultMaxMetadataBytes
	}
	return l, nil
}

// Normalize validates the request, enforces the pixel and metadata budgets and
// copies the source image into a canonical RGBA image.
//
// It accepts grayscale, paletted, NRGBA and premultiplied RGBA images, images
// with non-zero bounds and sub-images. Alpha is premultiplied during the copy,
// exactly like the legacy Analyzer path. The returned image is an owned copy:
// later mutations of the source do not affect it.
//
// A request with both dimensions zero is rejected with ErrInvalidDimensions.
// With a single zero dimension the missing side defaults to the source
// dimension, so `{Height: 100}` asks for a full-width, 100px tall crop.
//
// Errors are stable sentinels:
//   - ErrNilImage, ErrEmptyImage, ErrUnreadablePixels
//   - ErrDimensionOverflow, ErrBudgetExhausted, ErrNegativeBudget
//   - ErrInvalidDimensions for invalid requested dimensions
//   - ErrInvalidMetadata for invalid or oversized metadata
func Normalize(req Request, limits Limits) (*NormalizedImage, error) {
	return normalizeContext(context.Background(), req, limits)
}

// normalizeContext is Normalize with cancellation support. Pixel copying is
// performed in horizontal stripes so a canceled or timed-out request stops
// promptly instead of finishing a multi-megapixel copy.
func normalizeContext(ctx context.Context, req Request, limits Limits) (*NormalizedImage, error) {
	limits, err := limits.resolve()
	if err != nil {
		return nil, err
	}
	if err := contextErr(ctx, "normalize"); err != nil {
		return nil, err
	}

	if len(req.Metadata) > limits.MaxMetadataBytes {
		return nil, newOpError("normalize", ErrInvalidMetadata, wrapf("metadata has %d bytes, limit %d", len(req.Metadata), limits.MaxMetadataBytes))
	}
	if !utf8.ValidString(req.Metadata) {
		return nil, newOpError("normalize", ErrInvalidMetadata, "metadata is not valid UTF-8")
	}

	if req.Image == nil {
		return nil, newOpError("normalize", ErrNilImage, "")
	}

	srcBounds, err := safeBounds(req.Image)
	if err != nil {
		return nil, err
	}
	width, height, err := canonicalDimensions(srcBounds)
	if err != nil {
		return nil, err
	}

	pixels := int64(width) * int64(height)
	if pixels > limits.MaxPixels {
		return nil, newOpError("normalize", ErrBudgetExhausted, wrapf("image has %d pixels, budget %d", pixels, limits.MaxPixels))
	}

	targetWidth, targetHeight, err := fitTarget(req.Width, req.Height, width, height)
	if err != nil {
		return nil, err
	}

	dst, err := allocateRGBA(width, height)
	if err != nil {
		return nil, err
	}
	if err := copyPixels(ctx, dst, req.Image, srcBounds); err != nil {
		return nil, err
	}
	if err := contextErr(ctx, "normalize"); err != nil {
		return nil, err
	}

	return &NormalizedImage{
		RGBA:         dst,
		Width:        width,
		Height:       height,
		TargetWidth:  targetWidth,
		TargetHeight: targetHeight,
		Aspect:       float64(targetWidth) / float64(targetHeight),
		Origin:       srcBounds.Min,
		Metadata:     req.Metadata,
	}, nil
}

// safeBounds calls Bounds and converts a panic from a broken image
// implementation into ErrUnreadablePixels.
func safeBounds(img image.Image) (bounds image.Rectangle, err error) {
	defer func() {
		if r := recover(); r != nil {
			bounds = image.Rectangle{}
			err = newOpError("normalize", ErrUnreadablePixels, wrapf("bounds panic: %v", r))
		}
	}()
	bounds = img.Bounds()
	return bounds, nil
}

// canonicalDimensions validates bounds and returns their integer dimensions.
func canonicalDimensions(b image.Rectangle) (int, int, error) {
	if b.Max.X < b.Min.X || b.Max.Y < b.Min.Y {
		return 0, 0, newOpError("normalize", ErrEmptyImage, wrapf("inverted bounds %v", b))
	}
	dx := int64(b.Max.X) - int64(b.Min.X)
	dy := int64(b.Max.Y) - int64(b.Min.Y)
	if dx < 0 || dy < 0 {
		return 0, 0, newOpError("normalize", ErrDimensionOverflow, wrapf("bounds %v overflow", b))
	}
	if dx == 0 || dy == 0 {
		return 0, 0, newOpError("normalize", ErrEmptyImage, wrapf("bounds %v have no pixels", b))
	}
	if dx > math.MaxInt64/dy {
		return 0, 0, newOpError("normalize", ErrDimensionOverflow, wrapf("bounds %v pixel count overflows", b))
	}
	platformMaxInt := int64(^uint(0) >> 1)
	if dx > platformMaxInt || dy > platformMaxInt {
		return 0, 0, newOpError("normalize", ErrDimensionOverflow, wrapf("bounds %v exceed int", b))
	}
	return int(dx), int(dy), nil
}

// fitTarget validates the requested box and fits it into the source image while
// preserving the requested aspect ratio. A zero dimension defaults to the
// source dimension on that axis.
func fitTarget(reqWidth, reqHeight, srcWidth, srcHeight int) (int, int, error) {
	if reqWidth == 0 && reqHeight == 0 {
		return 0, 0, newOpError("normalize", ErrInvalidDimensions, "both width and height are zero")
	}
	if reqWidth < 0 || reqHeight < 0 {
		return 0, 0, newOpError("normalize", ErrInvalidDimensions, wrapf("negative dimensions %dx%d", reqWidth, reqHeight))
	}
	if int64(reqWidth) > maxTargetDimension || int64(reqHeight) > maxTargetDimension {
		return 0, 0, newOpError("normalize", ErrDimensionOverflow, wrapf("requested dimensions %dx%d exceed %d", reqWidth, reqHeight, maxTargetDimension))
	}

	if reqWidth == 0 {
		reqWidth = srcWidth
	}
	if reqHeight == 0 {
		reqHeight = srcHeight
	}

	aspect := float64(reqWidth) / float64(reqHeight)
	if math.IsNaN(aspect) || math.IsInf(aspect, 0) || aspect <= 0 {
		return 0, 0, newOpError("normalize", ErrInvalidDimensions, wrapf("invalid aspect ratio from %dx%d", reqWidth, reqHeight))
	}

	cropWidth := srcWidth
	cropHeight := int(math.Floor(float64(srcWidth)/aspect + 0.5))
	if cropHeight > srcHeight {
		cropHeight = srcHeight
		cropWidth = int(math.Floor(float64(srcHeight)*aspect + 0.5))
	}
	if cropWidth < 1 {
		cropWidth = 1
	}
	if cropHeight < 1 {
		cropHeight = 1
	}
	if cropWidth > srcWidth {
		cropWidth = srcWidth
	}
	if cropHeight > srcHeight {
		cropHeight = srcHeight
	}
	return cropWidth, cropHeight, nil
}

// allocateRGBA allocates a canonical RGBA image and turns allocator panics into
// ErrDimensionOverflow so callers never see a half-initialized result.
func allocateRGBA(width, height int) (img *image.RGBA, err error) {
	defer func() {
		if r := recover(); r != nil {
			img = nil
			err = newOpError("normalize", ErrDimensionOverflow, wrapf("cannot allocate %dx%d RGBA: %v", width, height, r))
		}
	}()
	return image.NewRGBA(image.Rect(0, 0, width, height)), nil
}

// pixelStripeRows is the number of source rows copied between cancellation
// checks. It keeps cancellation latency low without dominating the copy loop
// with context lookups.
const pixelStripeRows = 64

// copyPixels copies the source bounds into dst at the origin in stripes and
// converts a panic from a broken image implementation into ErrUnreadablePixels.
func copyPixels(ctx context.Context, dst *image.RGBA, src image.Image, srcBounds image.Rectangle) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = newOpError("normalize", ErrUnreadablePixels, wrapf("pixel read panic: %v", r))
		}
	}()
	for y := srcBounds.Min.Y; y < srcBounds.Max.Y; y += pixelStripeRows {
		if err := contextErr(ctx, "normalize"); err != nil {
			return err
		}
		endY := y + pixelStripeRows
		if endY > srcBounds.Max.Y {
			endY = srcBounds.Max.Y
		}
		srcStrip := image.Rect(srcBounds.Min.X, y, srcBounds.Max.X, endY)
		dstStrip := image.Rect(0, y-srcBounds.Min.Y, srcBounds.Dx(), endY-srcBounds.Min.Y)
		draw.Draw(dst, dstStrip, src, srcStrip.Min, draw.Src)
	}
	return nil
}

// digest returns a deterministic content fingerprint of the canonical image.
func (n *NormalizedImage) digest() string {
	return digestPixels(n.RGBA)
}

// origin returns the source bounds origin; canonical coordinates are relative
// to it.
func (n *NormalizedImage) originPoint() image.Point {
	if n == nil {
		return image.Point{}
	}
	return n.Origin
}

// String implements fmt.Stringer for diagnostics.
func (n *NormalizedImage) String() string {
	if n == nil {
		return "<nil>"
	}
	return fmt.Sprintf("smartcrop.NormalizedImage{%dx%d target=%dx%d}", n.Width, n.Height, n.TargetWidth, n.TargetHeight)
}
