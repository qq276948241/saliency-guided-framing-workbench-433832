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
	"fmt"
)

// Stable sentinel errors returned by the orchestration layer. Every failure
// path returns one of these (possibly wrapped in an *OpError) so callers can
// branch on errors.Is without matching error strings.
var (
	// ErrInvalidRequest gets returned when a Request cannot be normalized.
	ErrInvalidRequest = errors.New("invalid crop request")

	// ErrNilImage gets returned when a nil image is handed to the orchestrator.
	ErrNilImage = errors.New("nil image")

	// ErrEmptyImage gets returned for images without any pixels.
	ErrEmptyImage = errors.New("empty image")

	// ErrUnreadablePixels gets returned when a pixel cannot be read. It is also
	// returned when a broken image implementation panics while being sampled.
	ErrUnreadablePixels = errors.New("unreadable pixels")

	// ErrDimensionOverflow gets returned when dimensions or the pixel budget
	// would overflow or exceed the configured maximum.
	ErrDimensionOverflow = errors.New("dimension overflow")

	// ErrNegativeBudget gets returned when a negative candidate or pixel budget
	// is supplied.
	ErrNegativeBudget = errors.New("negative budget")

	// ErrBudgetExhausted gets returned when candidate generation needs more
	// candidates than the configured budget permits. No partial result is
	// returned in that case.
	ErrBudgetExhausted = errors.New("candidate budget exhausted")

	// ErrCanceled gets returned when the caller's context was canceled.
	ErrCanceled = errors.New("operation canceled")

	// ErrDeadlineExceeded gets returned when the configured timeout expired.
	ErrDeadlineExceeded = errors.New("deadline exceeded")

	// ErrInvalidConfig gets returned for malformed orchestrator or scoring
	// configurations.
	ErrInvalidConfig = errors.New("invalid configuration")

	// ErrCandidateFailed gets returned when scoring a single candidate failed.
	// The orchestrator discards every partial result in that case.
	ErrCandidateFailed = errors.New("candidate scoring failed")

	// ErrNoCandidates gets returned when no legal candidate rectangle could be
	// produced for the supplied image and request.
	ErrNoCandidates = errors.New("no valid candidates")

	// ErrInvalidMetadata gets returned for metadata that is not valid UTF-8 or
	// exceeds the metadata size limit.
	ErrInvalidMetadata = errors.New("invalid metadata")
)

// OpError adds operation context and diagnostics to a stable sentinel error.
type OpError struct {
	Op     string
	Err    error
	Detail string
}

// Error implements the error interface.
func (e *OpError) Error() string {
	if e.Detail == "" {
		return "smartcrop: " + e.Op + ": " + e.Err.Error()
	}
	return "smartcrop: " + e.Op + ": " + e.Err.Error() + ": " + e.Detail
}

// Unwrap exposes the underlying sentinel error to errors.Is/errors.As.
func (e *OpError) Unwrap() error {
	return e.Err
}

// Is maps the package level cancellation sentinels onto the equivalent
// context errors, so both errors.Is(err, ErrCanceled) and
// errors.Is(err, context.Canceled) work.
func (e *OpError) Is(target error) bool {
	switch {
	case target == context.Canceled && e.Err == ErrCanceled:
		return true
	case target == context.DeadlineExceeded && e.Err == ErrDeadlineExceeded:
		return true
	}
	return false
}

// newOpError constructs an *OpError. A nil sentinel is replaced with a generic
// error so diagnostics never lose the underlying message.
func newOpError(op string, err error, detail string) error {
	if err == nil {
		err = errors.New("unknown error")
	}
	return &OpError{Op: op, Err: err, Detail: detail}
}

// contextErr converts the state of ctx into one of the stable cancellation
// sentinels. It returns nil while ctx is still alive.
func contextErr(ctx context.Context, op string) error {
	if ctx == nil {
		return nil
	}
	switch ctx.Err() {
	case context.Canceled:
		return newOpError(op, ErrCanceled, "")
	case context.DeadlineExceeded:
		return newOpError(op, ErrDeadlineExceeded, "")
	default:
		return nil
	}
}

// wrapf is a tiny helper used for diagnostics that must remain stable.
func wrapf(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}
