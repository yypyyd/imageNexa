package handler

import (
	"bytes"
	"encoding/base64"
	"mime/multipart"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestReadMultipartMedia(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	video, err := writer.CreateFormFile("reference_videos", "source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	mp4 := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2'}
	_, _ = video.Write(mp4)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest("POST", "/v1/videos", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if err := request.ParseMultipartForm(1 << 20); err != nil {
		t.Fatal(err)
	}
	context, _ := gin.CreateTestContext(httptest.NewRecorder())
	context.Request = request

	media, err := readMultipartMedia(context, 1<<20, "video", "reference_videos")
	if err != nil || len(media) != 1 {
		t.Fatalf("readMultipartMedia() = %#v, %v", media, err)
	}
	if media[0].ContentType != "video/mp4" || !bytes.Equal(media[0].Data, mp4) {
		t.Fatalf("decoded multipart media = %#v", media[0])
	}
}

func TestDecodeJSONMedia(t *testing.T) {
	mp4 := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'm', 'p', '4', '2'}
	raw := base64.StdEncoding.EncodeToString(mp4)
	media, err := decodeJSONMedia([]string{raw}, "video/mp4")
	if err != nil || len(media) != 1 {
		t.Fatalf("decodeJSONMedia() = %#v, %v", media, err)
	}
	if media[0].ContentType != "video/mp4" || !bytes.Equal(media[0].Data, mp4) {
		t.Fatalf("decoded JSON media = %#v", media[0])
	}

	wav := []byte{'R', 'I', 'F', 'F', 4, 0, 0, 0, 'W', 'A', 'V', 'E'}
	dataURI := "data:audio/wav;base64," + base64.StdEncoding.EncodeToString(wav)
	media, err = decodeJSONMedia([]string{dataURI}, "audio/mpeg")
	if err != nil || len(media) != 1 || media[0].ContentType != "audio/wav" {
		t.Fatalf("data URI decode = %#v, %v", media, err)
	}
}

func TestDecodeJSONImagesValidatesMagicAndNormalizesDataURI(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	input := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	images, err := decodeJSONImages([]string{input})
	if err != nil || len(images) != 1 {
		t.Fatalf("decodeJSONImages() = %#v, %v", images, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(images[0])
	if err != nil || !bytes.Equal(decoded, png) {
		t.Fatalf("normalized image = %x, %v", decoded, err)
	}

	spoof := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("not an image"))
	if _, err := decodeJSONImages([]string{spoof}); err == nil {
		t.Fatal("spoofed JSON input_reference was accepted")
	}
}

func TestReferenceMediaRejectsSpoofedTypes(t *testing.T) {
	for _, test := range []struct {
		kind string
		data []byte
	}{
		{kind: "image", data: []byte("not an image")},
		{kind: "video", data: []byte("not an mp4")},
		{kind: "audio", data: []byte("not audio")},
	} {
		if contentType, err := detectReferenceMediaType(test.data, test.kind); err == nil {
			t.Fatalf("%s spoof accepted as %q", test.kind, contentType)
		}
	}
	spoof := "data:audio/wav;base64," + base64.StdEncoding.EncodeToString([]byte("plain text"))
	if _, err := decodeJSONMedia([]string{spoof}, "audio/mpeg"); err == nil {
		t.Fatal("spoofed JSON audio was accepted")
	}
}

func TestReferenceMediaRejectsAVIFUntilItCanBeTranscoded(t *testing.T) {
	avif := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f', 0, 0, 0, 0, 'a', 'v', 'i', 'f'}
	if contentType, err := detectReferenceMediaType(avif, "image"); err == nil {
		t.Fatalf("AVIF accepted as %q without a decode/transcode path", contentType)
	}
	if _, err := detectReferenceMediaType(avif, "video"); err == nil {
		t.Fatal("AVIF was accepted as video")
	}
}

func TestISOBaseMediaCannotCrossReferenceKinds(t *testing.T) {
	video := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 0, 0, 0, 0, 'a', 'v', 'c', '1'}
	audio := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'M', '4', 'A', ' ', 0, 0, 0, 0, 'm', 'p', '4', 'a'}
	if contentType, err := detectReferenceMediaType(video, "video"); err != nil || contentType != "video/mp4" {
		t.Fatalf("video magic = %q, %v", contentType, err)
	}
	if _, err := detectReferenceMediaType(video, "audio"); err == nil {
		t.Fatal("video MP4 was accepted as audio")
	}
	if contentType, err := detectReferenceMediaType(audio, "audio"); err != nil || contentType != "audio/mp4" {
		t.Fatalf("audio magic = %q, %v", contentType, err)
	}
	if _, err := detectReferenceMediaType(audio, "video"); err == nil {
		t.Fatal("M4A was accepted as video")
	}
	ambiguous := []byte{0, 0, 0, 16, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm', 0, 0, 0, 0}
	if _, err := detectReferenceMediaType(ambiguous, "video"); err == nil {
		t.Fatal("ambiguous ISO-BMFF was accepted as video")
	}
	if _, err := detectReferenceMediaType(ambiguous, "audio"); err == nil {
		t.Fatal("ambiguous ISO-BMFF was accepted as audio")
	}
}
