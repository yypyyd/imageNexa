package service

import "testing"

func TestNormalizeVideoRange(t *testing.T) {
	for _, test := range []struct {
		input string
		want  string
		ok    bool
	}{
		{"", "", true},
		{"bytes=0-", "bytes=0-", true},
		{"Bytes=1024-2047", "bytes=1024-2047", true},
		{"bytes=-500", "bytes=-500", true},
		{"bytes=2048-1024", "", false},
		{"bytes=0-1,3-4", "", false},
		{"items=0-1", "", false},
		{"bytes=-0", "", false},
	} {
		got, err := normalizeVideoRange(test.input)
		if (err == nil) != test.ok || got != test.want {
			t.Fatalf("input=%q got=%q err=%v", test.input, got, err)
		}
	}
}
