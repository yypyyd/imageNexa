package handler

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"backend/internal/service"
	"github.com/gin-gonic/gin"
)

const (
	maxGenerateMultipartBytes  = 320 * 1024 * 1024
	maxImageEditMultipartBytes = 128 * 1024 * 1024
	maxImageJSONBytes          = 2 * 1024 * 1024
	maxImageEditFiles          = 14
)

var errReferenceMediaTooLarge = errors.New("reference media is empty or too large")

func parseFormBool(v string) bool {
	b, _ := strconv.ParseBool(strings.TrimSpace(v))
	return b
}

func decodeJSONMedia(inputs []string, defaultContentType string) ([]service.MediaReference, error) {
	out := make([]service.MediaReference, 0, len(inputs))
	for _, raw := range inputs {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		expectedKind := strings.ToLower(strings.TrimSpace(strings.SplitN(defaultContentType, "/", 2)[0]))
		if strings.HasPrefix(value, "data:") {
			parts := strings.SplitN(value, ",", 2)
			if len(parts) != 2 || !strings.Contains(parts[0], ";base64") {
				return nil, errors.New("invalid media data URI")
			}
			value = parts[1]
		}
		maxBytes := 50 << 20
		switch expectedKind {
		case "image":
			maxBytes = 20 << 20
		case "video":
			maxBytes = 200 << 20
		}
		if len(value) > ((maxBytes+2)/3)*4+4 {
			return nil, errReferenceMediaTooLarge
		}
		data, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(value)
		}
		if err != nil || len(data) == 0 {
			return nil, errors.New("invalid media encoding")
		}
		contentType, err := detectReferenceMediaType(data, expectedKind)
		if err != nil {
			return nil, err
		}
		out = append(out, service.MediaReference{Data: data, ContentType: contentType})
	}
	return out, nil
}

// decodeJSONImages applies the same magic-byte validation as multipart image
// uploads before handing normalized base64 payloads to provider adapters. The
// data-URI media type is never trusted on its own.
func decodeJSONImages(inputs []string) ([]string, error) {
	media, err := decodeJSONMedia(inputs, "image/png")
	if err != nil {
		return nil, err
	}
	images := make([]string, 0, len(media))
	for _, item := range media {
		images = append(images, base64.StdEncoding.EncodeToString(item.Data))
	}
	return images, nil
}

func readMultipartImagesStrict(c *gin.Context, keys ...string) ([]string, error) {
	refs, err := readMultipartMedia(c, 20<<20, "image", keys...)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !strings.HasPrefix(ref.ContentType, "image/") || ref.ContentType == "image/svg+xml" {
			return nil, errors.New("reference image must be an image")
		}
		out = append(out, base64.StdEncoding.EncodeToString(ref.Data))
	}
	return out, nil
}

func readMultipartMedia(c *gin.Context, maxBytes int64, expectedKind string, keys ...string) ([]service.MediaReference, error) {
	form := c.Request.MultipartForm
	if form == nil {
		return nil, nil
	}
	var out []service.MediaReference
	for _, key := range keys {
		for _, fh := range form.File[key] {
			if len(out) >= maxImageEditFiles {
				return nil, errors.New("too many reference media files")
			}
			f, err := fh.Open()
			if err != nil {
				return nil, err
			}
			data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
			_ = f.Close()
			if readErr != nil {
				return nil, readErr
			}
			if len(data) == 0 || int64(len(data)) > maxBytes {
				return nil, errReferenceMediaTooLarge
			}
			contentType, typeErr := detectReferenceMediaType(data, expectedKind)
			if typeErr != nil {
				return nil, typeErr
			}
			out = append(out, service.MediaReference{Data: data, ContentType: contentType, Filename: fh.Filename})
		}
	}
	return out, nil
}

func detectReferenceMediaType(data []byte, expectedKind string) (string, error) {
	if len(data) == 0 {
		return "", errors.New("reference media is empty")
	}
	expectedKind = strings.ToLower(strings.TrimSpace(expectedKind))
	detected := strings.ToLower(strings.TrimSpace(strings.SplitN(http.DetectContentType(data[:min(len(data), 512)]), ";", 2)[0]))
	switch expectedKind {
	case "image":
		switch detected {
		case "image/jpeg", "image/png", "image/gif", "image/webp":
			return detected, nil
		}
	case "video":
		if isoMediaBrand(data, "qt  ") {
			return "video/quicktime", nil
		}
		if isoMediaBrand(data, "mp41", "mp42", "avc1", "M4V ", "M4VH", "M4VP", "dash", "MSNV", "3gp4", "3gp5", "3g2a") {
			return "video/mp4", nil
		}
	case "audio":
		if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
			return "audio/wav", nil
		}
		if len(data) >= 3 && string(data[:3]) == "ID3" {
			return "audio/mpeg", nil
		}
		if len(data) >= 2 && data[0] == 0xff {
			if data[1]&0xf6 == 0xf0 {
				return "audio/aac", nil
			}
			if data[1]&0xe0 == 0xe0 && data[1]&0x06 != 0 {
				return "audio/mpeg", nil
			}
		}
		if isoMediaBrand(data, "M4A ", "M4B ", "M4P ", "mp4a") {
			return "audio/mp4", nil
		}
	}
	return "", errors.New("reference media content does not match its field")
}

func isISOBaseMedia(data []byte) bool {
	return len(data) >= 12 && string(data[4:8]) == "ftyp"
}

func isoMediaBrand(data []byte, brands ...string) bool {
	if !isISOBaseMedia(data) {
		return false
	}
	wanted := make(map[string]struct{}, len(brands))
	for _, brand := range brands {
		wanted[brand] = struct{}{}
	}
	if _, ok := wanted[string(data[8:12])]; ok {
		return true
	}
	// Compatible brands begin after the major brand and minor version. Only
	// inspect complete four-byte entries inside the declared ftyp box.
	boxSize := int(data[0])<<24 | int(data[1])<<16 | int(data[2])<<8 | int(data[3])
	limit := min(len(data), 128)
	if boxSize >= 16 {
		limit = min(limit, boxSize)
	}
	for offset := 16; offset+4 <= limit; offset += 4 {
		if _, ok := wanted[string(data[offset:offset+4])]; ok {
			return true
		}
	}
	return false
}
