package service

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"testing"

	"backend/internal/netguard"
)

func TestDecodeReferenceImagesNormalizesSupportedMagicToPNG(t *testing.T) {
	picture := image.NewRGBA(image.Rect(0, 0, 2, 2))
	picture.Set(0, 0, color.RGBA{R: 255, A: 255})
	encode := func(fn func(*bytes.Buffer) error) []byte {
		t.Helper()
		var output bytes.Buffer
		if err := fn(&output); err != nil {
			t.Fatal(err)
		}
		return output.Bytes()
	}
	fixtures := map[string][]byte{
		"png": encode(func(output *bytes.Buffer) error { return png.Encode(output, picture) }),
		"jpeg": encode(func(output *bytes.Buffer) error {
			return jpeg.Encode(output, picture, &jpeg.Options{Quality: 85})
		}),
		"gif": encode(func(output *bytes.Buffer) error { return gif.Encode(output, picture, nil) }),
	}
	webp, err := base64.StdEncoding.DecodeString("UklGRiYAAABXRUJQVlA4IBoAAAAwAQCdASoBAAEAAgA0JZwAA3AA/vpopw8gAA==")
	if err != nil {
		t.Fatal(err)
	}
	fixtures["webp"] = webp

	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			// The deliberately false declaration proves the service trusts magic.
			input := "data:text/plain;base64," + base64.StdEncoding.EncodeToString(fixture)
			decoded, decodeErr := decodeReferenceImages([]string{input}, 1)
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if len(decoded) != 1 || netguard.DetectMediaType(decoded[0]) != "image/png" {
				t.Fatalf("normalized output = %d items, type %q", len(decoded), netguard.DetectMediaType(decoded[0]))
			}
		})
	}
}

func TestDecodeReferenceImagesRejectsSpoofedAndUnsupportedPayloads(t *testing.T) {
	tests := map[string][]byte{
		"html disguised as png": []byte("<html>not an image</html>"),
		"avif without decoder":  append(append([]byte{0, 0, 0, 24}, []byte("ftypavif")...), make([]byte, 32)...),
	}
	for name, fixture := range tests {
		t.Run(name, func(t *testing.T) {
			input := "data:image/png;base64," + base64.StdEncoding.EncodeToString(fixture)
			if _, err := decodeReferenceImages([]string{input}, 1); !errors.Is(err, ErrUnsupportedParams) {
				t.Fatalf("decodeReferenceImages() error = %v, want ErrUnsupportedParams", err)
			}
		})
	}
}
