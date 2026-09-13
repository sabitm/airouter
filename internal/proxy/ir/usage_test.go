package ir

import "testing"

func TestClampCacheTokens(t *testing.T) {
	cases := []struct {
		name      string
		input     int
		read      int
		write     int
		wantRead  int
		wantWrite int
	}{
		{"zeros", 0, 0, 0, 0, 0},
		{"valid subsets", 2600, 2000, 400, 2000, 400},
		{"read only", 100, 40, 0, 40, 0},
		{"write only", 100, 0, 25, 0, 25},
		{"negative buckets", 50, -3, -9, 0, 0},
		{"negative input zeros cache", -1, 4, 5, 0, 0},
		{"read exceeds total", 10, 12, 3, 10, 0},
		{"sum exceeds prefers read", 100, 80, 50, 80, 20},
		{"write exceeds remainder", 10, 8, 8, 8, 2},
		{"both exceed total", 5, 100, 100, 5, 0},
		{"exact total", 10, 6, 4, 6, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRead, gotWrite := ClampCacheTokens(tc.input, tc.read, tc.write)
			if gotRead != tc.wantRead || gotWrite != tc.wantWrite {
				t.Fatalf("ClampCacheTokens(%d,%d,%d) = %d,%d, want %d,%d",
					tc.input, tc.read, tc.write, gotRead, gotWrite, tc.wantRead, tc.wantWrite)
			}
			if tc.input >= 0 && gotRead+gotWrite > tc.input {
				t.Fatalf("cache sum %d exceeds input %d", gotRead+gotWrite, tc.input)
			}
		})
	}
}
