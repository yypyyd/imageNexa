package netguard

import (
	"bytes"
	"io"
	"net/http"
	"testing"
)

func TestGuardPartialResponseAcceptsNonZeroVideoRange(t *testing.T) {
	payload := bytes.Repeat([]byte{0x5a}, 128)
	resp := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Header: http.Header{
			"Content-Type":  []string{"video/mp4"},
			"Content-Range": []string{"bytes 1024-1151/4096"},
		},
	}
	body, contentType, err := GuardPartialResponse(resp, MediaVideo, 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "video/mp4" || !bytes.Equal(got, payload) {
		t.Fatalf("contentType=%q bytes=%d", contentType, len(got))
	}
}

func TestGuardPartialResponseChecksZeroRangeMagic(t *testing.T) {
	payload := append([]byte{
		0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm',
		0, 0, 0, 0, 'm', 'p', '4', '2', 'i', 's', 'o', 'm',
	}, bytes.Repeat([]byte{0}, 1000)...)
	resp := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Body:          io.NopCloser(bytes.NewReader(payload)),
		ContentLength: int64(len(payload)),
		Header: http.Header{
			"Content-Type":  []string{"video/mp4"},
			"Content-Range": []string{"bytes 0-1023/4096"},
		},
	}
	body, contentType, err := GuardPartialResponse(resp, MediaVideo, 8192)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if contentType != "video/mp4" {
		t.Fatalf("contentType=%q", contentType)
	}
}

func TestGuardPartialResponseRejectsInconsistentRange(t *testing.T) {
	resp := &http.Response{
		StatusCode:    http.StatusPartialContent,
		Body:          io.NopCloser(bytes.NewReader(make([]byte, 10))),
		ContentLength: 10,
		Header: http.Header{
			"Content-Type":  []string{"video/mp4"},
			"Content-Range": []string{"bytes 100-120/8192"},
		},
	}
	if _, _, err := GuardPartialResponse(resp, MediaVideo, 8192); err == nil {
		t.Fatal("expected mismatched partial response to fail")
	}
}
