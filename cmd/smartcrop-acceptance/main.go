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

// Command smartcrop-acceptance runs the offline acceptance suite for the
// saliency crop orchestration layer. It never touches the network: all sample
// images are generated from code. It prints a JSON report and exits with
// status 1 when any check fails.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/muesli/smartcrop"
	"github.com/muesli/smartcrop/nfnt"
)

func main() {
	seed := flag.Uint64("seed", 1, "deterministic candidate seed")
	timeout := flag.Duration("timeout", 30*time.Second, "overall acceptance timeout")
	out := flag.String("out", "", "optional path for the JSON report")
	repeat := flag.Int("repeat", 2, "number of runs used for the summary comparison (>= 2)")
	flag.Parse()

	if *repeat < 2 {
		fmt.Fprintln(os.Stderr, "repeat must be >= 2")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	cfg := smartcrop.Config{Seed: *seed}
	legacyImage := smartcrop.OfflineSamples()[0].Image
	legacyRect, err := smartcrop.NewAnalyzer(nfnt.NewDefaultResizer()).FindBestCrop(legacyImage, 100, 50)
	if err != nil {
		fmt.Fprintf(os.Stderr, "legacy analyzer failed: %v\n", err)
		os.Exit(1)
	}

	reports := make([]smartcrop.AcceptanceReport, 0, *repeat)
	for i := 0; i < *repeat; i++ {
		rep := smartcrop.RunAcceptance(ctx, smartcrop.AcceptanceOptions{
			Config: cfg,
			Legacy: &smartcrop.LegacyExpectation{
				Image:    legacyImage,
				Width:    100,
				Height:   50,
				Expected: legacyRect,
			},
		})
		if err := ctx.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "acceptance aborted: %v\n", err)
			os.Exit(1)
		}
		reports = append(reports, rep)
	}

	for i := 1; i < len(reports); i++ {
		if reports[i].Digest != reports[0].Digest || reports[i].Summary != reports[0].Summary {
			fmt.Fprintf(os.Stderr, "run %d summary differs from run 0\n", i)
			os.Exit(1)
		}
	}

	rep := reports[0]
	data, err := json.MarshalIndent(struct {
		smartcrop.AcceptanceReport
		Repeat int `json:"repeat"`
	}{AcceptanceReport: rep, Repeat: len(reports)}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot encode report: %v\n", err)
		os.Exit(1)
	}

	if *out != "" {
		if err := os.MkdirAll(filepath.Dir(*out), 0755); err != nil {
			fmt.Fprintf(os.Stderr, "cannot create output directory: %v\n", err)
			os.Exit(1)
		}
		if err := ioutil.WriteFile(*out, append(data, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "cannot write report: %v\n", err)
			os.Exit(1)
		}
	}

	fmt.Println(string(data))
	if !rep.OK() {
		os.Exit(1)
	}
}
