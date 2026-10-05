// Package upload implements resumable multipart uploads: the api hands out
// presigned part URLs, the client PUTs parts straight to the bucket and
// reports ETags, and the api completes the upload once every part is recorded.
package upload

import (
	"errors"
	"fmt"

	"github.com/ArianKhademi/rigforge/api/internal/config"
)

// This file is the pure part of the upload state machine: no I/O, so the rules
// (how a file is cut into parts, which part numbers are valid, when an upload
// may complete) are unit-tested without a database or a bucket.

var (
	ErrEmptyFile    = errors.New("file is empty")
	ErrTooManyParts = errors.New("file needs more parts than a multipart upload allows")
	ErrPartRange    = errors.New("part number out of range")
)

// PartCount is how many parts a file of size bytes needs: every part is
// partSize bytes except the last, which holds the remainder.
func PartCount(size, partSize int64) (int, error) {
	if size <= 0 {
		return 0, ErrEmptyFile
	}
	n := (size + partSize - 1) / partSize // ceiling division
	if n > config.MaxParts {
		return 0, ErrTooManyParts
	}
	return int(n), nil
}

// PartSpan returns the byte offset and length of part n (1-based, as in S3).
func PartSpan(size, partSize int64, partCount, n int) (offset, length int64, err error) {
	if n < 1 || n > partCount {
		return 0, 0, fmt.Errorf("%w: %d not in 1..%d", ErrPartRange, n, partCount)
	}
	offset = int64(n-1) * partSize
	length = partSize
	if n == partCount {
		length = size - offset
	}
	return offset, length, nil
}

// MissingParts lists the part numbers in 1..partCount that have no recorded
// ETag yet, in ascending order. An upload may complete only when this is empty.
func MissingParts(partCount int, recorded []int) []int {
	have := make(map[int]bool, len(recorded))
	for _, n := range recorded {
		have[n] = true
	}
	missing := []int{}
	for n := 1; n <= partCount; n++ {
		if !have[n] {
			missing = append(missing, n)
		}
	}
	return missing
}
