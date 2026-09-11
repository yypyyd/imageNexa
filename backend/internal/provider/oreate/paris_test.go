package oreate

import (
	"strings"
	"testing"
)

func TestIsBantiReportURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://banti.oreateai.com/dr", true},
		{"https://banti.oreateai.com/dr?x=1", true},
		{"https://banti-static.oreateai.com/o/static/banti_21a851acb0.js", false},
		{"https://banti-static.oreateai.com/dr", true},
		{"https://cdn.oreateai.com/dr", false},
		{"https://example.com/dr", false},
		{":", false},
	}
	for _, tc := range cases {
		if got := isBantiReportURL(tc.raw); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.raw, got, tc.want)
		}
	}
}

func TestParisScriptsUseLiveInstanceCache(t *testing.T) {
	t.Parallel()
	if !strings.Contains(parisReadyJS, "PARIS_INSTANCE_CACHE") || !strings.Contains(parisReadyJS, "sendBantiReport") {
		t.Fatal("paris ready expression is missing the current Oreate attachment points")
	}
	if !strings.Contains(parisHelperJS, "oreateParisInstance") || !strings.Contains(parisHelperJS, "oreateSendBantiReport") {
		t.Fatal("paris helper is missing the live instance lookup")
	}
	if strings.Contains(mintBantiEvaluateJS(), "paris_21a851acb0") || strings.Contains(inPageSubmitScript(`""`), "paris_21a851acb0") {
		t.Fatal("injected scripts still hard-code the retired Paris global")
	}
	if !strings.Contains(parisHelperJS, "oreateGetAcsToken") || !strings.Contains(parisHelperJS, "getAcsToken") {
		t.Fatal("paris helper is missing the official ACS token lookup")
	}
	script := inPageSubmitScript(`""`)
	for _, needle := range []string{`"JS-Token"`, `"Acs-Token"`, `mintJT("sse")`, `/oreate/sse/stream`} {
		if !strings.Contains(script, needle) {
			t.Fatalf("in-page submit is missing official risk header flow %q", needle)
		}
	}
	if strings.Contains(script, "/oreate/create/chat") || strings.Contains(script, `mintJT("create")`) || strings.Contains(script, "oreateCreateCaptcha") {
		t.Fatal("in-page submit still uses a retired create path")
	}
}
