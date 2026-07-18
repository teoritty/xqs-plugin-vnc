package transport

import "testing"

func TestSplitPayload(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		max  int
		want []int // expected chunk lengths
	}{
		{"empty", nil, 4, nil},
		{"exact multiple", []byte("abcdefgh"), 4, []int{4, 4}},
		{"remainder", []byte("abcdefghi"), 4, []int{4, 4, 1}},
		{"smaller than max", []byte("ab"), 4, []int{2}},
		{"zero max falls back to default", []byte("ab"), 0, []int{2}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitPayload(tc.in, tc.max)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d chunks, want %d (%v)", len(got), len(tc.want), got)
			}
			var reassembled []byte
			for i, c := range got {
				if len(c) != tc.want[i] {
					t.Fatalf("chunk %d: got len %d, want %d", i, len(c), tc.want[i])
				}
				if tc.max > 0 && len(c) > tc.max {
					t.Fatalf("chunk %d exceeds max %d", i, tc.max)
				}
				reassembled = append(reassembled, c...)
			}
			if string(reassembled) != string(tc.in) {
				t.Fatalf("reassembled %q, want %q", reassembled, tc.in)
			}
		})
	}
}
