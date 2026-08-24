package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestFacePanelTransformAppliesToAdobeAndOreateSeedance(t *testing.T) {
	for _, model := range []string{
		"firefly-seedance-2", "firefly-seedance-2-fast", "seedance20", "seedance20-fast", "sd2.0", "sd2.0-fast",
		"oreate-seedance-1.5-pro", "oreate-seedance-2.0-mini", "oreate-seedance-2.0-fast", "oreate-seedance-2.0", "oreate-seedance-2.5",
		" OREATE-SEEDANCE-2.0-MINI ",
	} {
		if !(&V1Service{}).shouldApplyReferenceGrid(context.Background(), model, false) {
			t.Fatalf("model %q should enable face-panel transform", model)
		}
	}
	for _, model := range []string{"firefly-kling-o3", "firefly-ray", "gpt-image-2", "runway-gen4", ""} {
		if (&V1Service{}).shouldApplyReferenceGrid(context.Background(), model, true) {
			t.Fatalf("model %q should not enable face-panel transform", model)
		}
	}
}

func TestApplyReferenceFaceSwapKeepsSingleReferenceCompatible(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 180, G: 180, B: 180, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, img); err != nil {
		t.Fatal(err)
	}
	raw := base64.StdEncoding.EncodeToString(encoded.Bytes())
	out, err := applyReferenceFaceSwap([]string{raw})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != raw {
		t.Fatalf("single reference should pass through unchanged: %#v", out)
	}
}

func TestDecodeReferenceImageAcceptsDataURI(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("payload"))
	out, err := decodeReferenceImage("data:image/png;base64," + raw)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "payload" {
		t.Fatalf("unexpected decoded payload %q", out)
	}
}

func TestVeilFaceCoversFaceRegionWithRedMesh(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 90, 90))
	for y := 0; y < 90; y++ {
		for x := 0; x < 90; x++ {
			src.SetRGBA(x, y, color.RGBA{R: 200, G: 200, B: 200, A: 255})
		}
	}
	out, err := veilFaceWithinImage(src, detectedFace{box: image.Rect(20, 30, 60, 70)})
	if err != nil {
		t.Fatal(err)
	}
	box := expandFacePanelBox(image.Rect(20, 30, 60, 70), src.Bounds())
	if box != image.Rect(23, 35, 57, 68) {
		t.Fatalf("veil box was not trimmed to the face: %v", box)
	}
	var red, total int
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			total++
			if got := out.RGBAAt(x, y); got.R == 255 && got.G == 0 && got.B == 0 {
				red++
			}
		}
	}
	if red*10 < total {
		t.Fatalf("veil covers %d of %d pixels, want visible mesh", red, total)
	}
	if got := out.RGBAAt(85, 85); got.R != 200 || got.G != 200 || got.B != 200 {
		t.Fatalf("pixel outside the veil was modified: %#v", got)
	}
}
