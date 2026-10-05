package upload

import (
	"errors"
	"reflect"
	"testing"
)

const mib = 1 << 20

func TestPartCount(t *testing.T) {
	cases := []struct {
		name     string
		size     int64
		partSize int64
		want     int
		wantErr  error
	}{
		{"one byte", 1, 64 * mib, 1, nil},
		{"exactly one part", 64 * mib, 64 * mib, 1, nil},
		{"one byte over", 64*mib + 1, 64 * mib, 2, nil},
		// The resume test's file: 2.1 GiB in 64 MiB parts.
		{"2.1 GiB", 2254857830, 64 * mib, 34, nil},
		{"empty", 0, 64 * mib, 0, ErrEmptyFile},
		{"negative", -5, 64 * mib, 0, ErrEmptyFile},
		{"at the 10,000 part limit", 10000 * 5 * mib, 5 * mib, 10000, nil},
		{"over the 10,000 part limit", 10000*5*mib + 1, 5 * mib, 0, ErrTooManyParts},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PartCount(tc.size, tc.partSize)
			if !errors.Is(err, tc.wantErr) || got != tc.want {
				t.Fatalf("PartCount(%d, %d) = %d, %v; want %d, %v", tc.size, tc.partSize, got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestPartSpanCoversTheFileExactly(t *testing.T) {
	const size, partSize = int64(2254857830), int64(64 * mib)
	count, _ := PartCount(size, partSize)

	var next int64
	for n := 1; n <= count; n++ {
		offset, length, err := PartSpan(size, partSize, count, n)
		if err != nil {
			t.Fatal(err)
		}
		if offset != next {
			t.Fatalf("part %d starts at %d, want %d (parts must be contiguous)", n, offset, next)
		}
		if n < count && length != partSize {
			t.Fatalf("part %d is %d bytes; every part but the last must be %d", n, length, partSize)
		}
		next = offset + length
	}
	if next != size {
		t.Fatalf("parts cover %d bytes, file is %d", next, size)
	}
	if _, last, _ := PartSpan(size, partSize, count, count); last != size-int64(count-1)*partSize {
		t.Fatalf("last part is %d bytes", last)
	}
}

func TestPartSpanRejectsOutOfRange(t *testing.T) {
	for _, n := range []int{0, -1, 4} {
		if _, _, err := PartSpan(25, 10, 3, n); !errors.Is(err, ErrPartRange) {
			t.Errorf("PartSpan(part %d of 3) err = %v, want ErrPartRange", n, err)
		}
	}
}

func TestMissingParts(t *testing.T) {
	cases := []struct {
		count    int
		recorded []int
		want     []int
	}{
		{3, nil, []int{1, 2, 3}},
		{3, []int{3, 1}, []int{2}},        // recorded out of order
		{3, []int{2, 2, 2}, []int{1, 3}},  // a duplicate does not count twice
		{3, []int{1, 2, 3}, []int{}},      // complete
		{3, []int{1, 2, 9}, []int{3}},     // an out-of-range number cannot stand in for a real part
		{5, []int{5, 1, 3}, []int{2, 4}},  // gaps come back ascending
		{2, []int{0, -1, 7}, []int{1, 2}}, // garbage numbers are ignored
	}
	for _, tc := range cases {
		if got := MissingParts(tc.count, tc.recorded); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("MissingParts(%d, %v) = %v, want %v", tc.count, tc.recorded, got, tc.want)
		}
	}
}
