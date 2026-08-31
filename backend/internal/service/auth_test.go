package service

import "testing"

func TestParseBearerOpenAIShape(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "normal", header: "Bearer sk-test", want: "sk-test"},
		{name: "case insensitive scheme", header: "bearer sk-test", want: "sk-test"},
		{name: "missing", header: "", want: ""},
		{name: "wrong scheme", header: "Basic sk-test", want: ""},
		{name: "missing key", header: "Bearer", want: ""},
		{name: "extra field", header: "Bearer sk-test extra", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ParseBearer(test.header); got != test.want {
				t.Fatalf("ParseBearer(%q) = %q, want %q", test.header, got, test.want)
			}
		})
	}
}

func TestHashAPIKeyIsDeterministicAndDoesNotContainPlaintext(t *testing.T) {
	plain := "sk-sensitive-value"
	first := HashAPIKey(plain)
	second := HashAPIKey(plain)
	if first != second {
		t.Fatalf("HashAPIKey is not deterministic: %q != %q", first, second)
	}
	if first == plain || len(first) != len("sha256:")+64 {
		t.Fatalf("unexpected API key hash %q", first)
	}
}
