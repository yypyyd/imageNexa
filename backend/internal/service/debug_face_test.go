package service

import (
	"bytes"
	"image"
	"os"
	"testing"
)

// TestPrintPigoFace is a manual helper: drop a sample image at the path below to
// dump the detected face boxes. It skips when that image is absent.
func TestPrintPigoFace(t *testing.T) {
	const sample = "/tmp/codex-original.png"
	b, err := os.ReadFile(sample)
	if err != nil {
		t.Skipf("sample image %s not present", sample)
	}
	im, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	f, err := detectFaces(im)
	if err != nil {
		t.Fatal(err)
	}
	for i, face := range f {
		t.Logf("face[%d]=%+v", i, face)
	}
}
