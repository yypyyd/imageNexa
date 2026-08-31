package netguard

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestValidateAssetURLRejectsUnsafeTargets(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/image.png",
		"https://user:pass@example.com/image.png",
		"https://example.com:8443/image.png",
		"https://localhost/image.png",
		"https://127.0.0.1/image.png",
		"https://169.254.169.254/latest/meta-data",
		"https://100.64.0.1/image.png",
		"https://198.18.0.1/image.png",
		"https://192.0.2.1/image.png",
		"https://[::1]/image.png",
		"https://[2001:db8::1]/image.png",
		"https://[2002:7f00:1::1]/image.png",
		"https://[2001:0:4136:e378:8000:63bf:3fff:fdd2]/image.png",
	} {
		if _, err := ValidateAssetURL(context.Background(), raw, nil); !errors.Is(err, ErrUnsafeAssetURL) {
			t.Fatalf("ValidateAssetURL(%q) error = %v, want ErrUnsafeAssetURL", raw, err)
		}
	}
	if _, err := ValidateAssetURL(context.Background(), "https://example.com/image.png", []string{"grok.com"}); !errors.Is(err, ErrUnsafeAssetURL) {
		t.Fatalf("host allowlist error = %v, want ErrUnsafeAssetURL", err)
	}
}

func TestGuardResponseValidatesMagicAndBoundsChunkedBodies(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	resp := &http.Response{Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader(string(png))), ContentLength: -1}
	body, contentType, err := GuardResponse(resp, MediaImage, 16)
	if err != nil {
		t.Fatalf("GuardResponse() error = %v", err)
	}
	if contentType != "image/png" {
		t.Fatalf("content type = %q", contentType)
	}
	if _, err := io.ReadAll(body); !errors.Is(err, ErrAssetTooLarge) {
		t.Fatalf("bounded body error = %v, want ErrAssetTooLarge", err)
	}
	_ = body.Close()

	notImage := &http.Response{Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(strings.NewReader("<html>not an image</html>")), ContentLength: -1}
	if _, _, err := GuardResponse(notImage, MediaImage, MaxImageBytes); !errors.Is(err, ErrInvalidMedia) {
		t.Fatalf("invalid media error = %v, want ErrInvalidMedia", err)
	}
}

func TestGuardResponseRecognizesAVIFMagic(t *testing.T) {
	avif := append([]byte{0, 0, 0, 24}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 32)...)
	resp := &http.Response{
		Header:        http.Header{"Content-Type": []string{"image/avif"}},
		Body:          io.NopCloser(strings.NewReader(string(avif))),
		ContentLength: int64(len(avif)),
	}
	body, contentType, err := GuardResponse(resp, MediaImage, MaxImageBytes)
	if err != nil {
		t.Fatalf("GuardResponse(AVIF) error = %v", err)
	}
	defer body.Close()
	if contentType != "image/avif" {
		t.Fatalf("content type = %q, want image/avif", contentType)
	}
}

func TestGuardResponseUsesMagicWhenImageHeaderConflicts(t *testing.T) {
	avif := append([]byte{0, 0, 0, 24}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 32)...)
	resp := &http.Response{
		// A stale object-store header must not turn AVIF bytes into PNG metadata.
		Header:        http.Header{"Content-Type": []string{"image/png"}},
		Body:          io.NopCloser(strings.NewReader(string(avif))),
		ContentLength: int64(len(avif)),
	}
	body, contentType, err := GuardResponse(resp, MediaImage, MaxImageBytes)
	if err != nil {
		t.Fatalf("GuardResponse(conflicting AVIF) error = %v", err)
	}
	defer body.Close()
	if contentType != "image/avif" {
		t.Fatalf("content type = %q, want magic-detected image/avif", contentType)
	}
	if extension, ok := MediaExtension(contentType); !ok || extension != ".avif" {
		t.Fatalf("MediaExtension(%q) = %q, %v", contentType, extension, ok)
	}
}
