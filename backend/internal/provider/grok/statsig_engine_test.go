package grok

import "testing"

func TestIsObfuscatedSignerAcceptsEquivalentByteModuloForms(t *testing.T) {
	for _, decoder := range []string{"%256", "%0x100", "&255", "&0xFF"} {
		source := "String.fromCharCode(x" + decoder + ");value.charCodeAt(0)"
		if !isObfuscatedSigner(source) {
			t.Fatalf("decoder %q was not recognized", decoder)
		}
	}
}

func TestIsObfuscatedSignerRequiresStableStringDecoderMarkers(t *testing.T) {
	if isObfuscatedSigner("value%256") {
		t.Fatal("weak modulo-only candidate was accepted")
	}
}
