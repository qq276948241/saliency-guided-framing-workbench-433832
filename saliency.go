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
	"image"
	"math"
)

// scoringParams captures the tunables of the importance function. The legacy
// Analyzer uses legacyScoringParams, which keeps the historic arithmetic
// unchanged.
type scoringParams struct {
	detailWeight      float64
	skinBias          float64
	skinWeight        float64
	saturationBias    float64
	saturationWeight  float64
	edgeRadius        float64
	edgeWeight        float64
	outsideImportance float64
	ruleOfThirds      bool
}

var legacyScoringParams = scoringParams{
	detailWeight:      detailWeight,
	skinBias:          skinBias,
	skinWeight:        skinWeight,
	saturationBias:    saturationBias,
	saturationWeight:  saturationWeight,
	edgeRadius:        edgeRadius,
	edgeWeight:        edgeWeight,
	outsideImportance: outsideImportance,
	ruleOfThirds:      ruleOfThirds,
}

// totalScore mirrors Crop.totalScore without dividing by zero: legacy code
// divides by Dx()*Dy(), which panics for empty rectangles. Scoring never
// creates empty rectangles, and the guard keeps the function total.
func (s Score) totalScore(rect image.Rectangle) float64 {
	if rect.Dx() <= 0 || rect.Dy() <= 0 {
		return math.Inf(-1)
	}
	return s.total(rect)
}

func (s Score) total(rect image.Rectangle) float64 {
	return (s.Detail*detailWeight + s.Skin*skinWeight + s.Saturation*saturationWeight) / float64(rect.Dx()) / float64(rect.Dy())
}

// buildSaliency produces the read-only saliency map: the red channel encodes
// skin, green encodes detail (edges), blue encodes saturation. It is built once
// per request and never mutated afterwards, which is what makes parallel
// scoring deterministic and race-free.
func buildSaliency(img *image.RGBA) (*image.RGBA, error) {
	if img == nil {
		return nil, newOpError("saliency", ErrInvalidRequest, "nil RGBA image")
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return nil, newOpError("saliency", ErrEmptyImage, wrapf("bounds %v have no pixels", bounds))
	}
	o := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	edgeDetect(img, o)
	skinDetect(img, o)
	saturationDetect(img, o)
	return o, nil
}

// scoreFor is the parameterized version of the legacy score function. The
// arithmetic order is identical to the legacy implementation so a Config that
// matches the legacy defaults reproduces Analyzer's choice exactly.
func scoreFor(output *image.RGBA, crop Candidate, p scoringParams) Score {
	width := output.Bounds().Dx()
	height := output.Bounds().Dy()
	s := Score{}

	for y := 0; y <= height-scoreDownSample; y += scoreDownSample {
		for x := 0; x <= width-scoreDownSample; x += scoreDownSample {
			c := output.RGBAAt(x, y)
			r8 := float64(c.R)
			g8 := float64(c.G)
			b8 := float64(c.B)

			imp := importanceFor(crop.Rectangle, p, x, y)
			det := g8 / 255.0

			s.Skin += r8 / 255.0 * (det + p.skinBias) * imp
			s.Detail += det * imp
			s.Saturation += b8 / 255.0 * (det + p.saturationBias) * imp
		}
	}

	return s
}

// importanceFor is the parameterized importance function. It applies the
// edge penalty band of p.edgeRadius and the rule-of-thirds boost unless it is
// disabled. The expression order matches the legacy importance function.
func importanceFor(crop image.Rectangle, p scoringParams, x, y int) float64 {
	if crop.Min.X > x || x >= crop.Max.X || crop.Min.Y > y || y >= crop.Max.Y {
		return p.outsideImportance
	}

	xf := float64(x-crop.Min.X) / float64(crop.Dx())
	yf := float64(y-crop.Min.Y) / float64(crop.Dy())

	px := math.Abs(0.5-xf) * 2.0
	py := math.Abs(0.5-yf) * 2.0

	dx := math.Max(px-1.0+p.edgeRadius, 0.0)
	dy := math.Max(py-1.0+p.edgeRadius, 0.0)
	d := (dx*dx + dy*dy) * p.edgeWeight

	s := 1.41 - math.Sqrt(px*px+py*py)
	if p.ruleOfThirds {
		s += (math.Max(0.0, s+d+0.5) * 1.2) * (thirds(px) + thirds(py))
	}

	return s + d
}
