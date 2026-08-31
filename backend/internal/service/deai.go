package service

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"time"

	_ "image/gif"

	_ "golang.org/x/image/webp"
)

// applyDeAI post-processes a generated image to weaken AI-detection artifacts:
// a small edge crop (kills border fingerprints), per-pixel low-amplitude noise,
// a subtle brightness/contrast jitter, and a JPEG re-encode round-trip that
// both introduces natural compression statistics and strips any embedded
// metadata/watermark chunks. Output is PNG (matching the stored content type).
func applyDeAI(b []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	// Crop a sliver off each edge (~0.3% of the dimension, 2–12px).
	cropX := clampInt(w*3/1000, 2, 12)
	cropY := clampInt(h*3/1000, 2, 12)
	if w <= cropX*4 || h <= cropY*4 {
		cropX, cropY = 0, 0
	}
	x0, y0 := bounds.Min.X+cropX, bounds.Min.Y+cropY
	x1, y1 := bounds.Max.X-cropX, bounds.Max.Y-cropY

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	// Subtle tone jitter: contrast ±2%, brightness ±2 levels.
	contrast := 1.0 + (rng.Float64()-0.5)*0.04
	brightness := (rng.Float64() - 0.5) * 4.0

	out := image.NewRGBA(image.Rect(0, 0, x1-x0, y1-y0))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			out.SetRGBA(x-x0, y-y0, color.RGBA{
				R: jitterChannel(r, contrast, brightness, rng),
				G: jitterChannel(g, contrast, brightness, rng),
				B: jitterChannel(bl, contrast, brightness, rng),
				A: uint8(a >> 8),
			})
		}
	}

	// JPEG round-trip: natural compression statistics + strips metadata.
	var jbuf bytes.Buffer
	if err := jpeg.Encode(&jbuf, out, &jpeg.Options{Quality: 93}); err != nil {
		return nil, err
	}
	rt, err := jpeg.Decode(bytes.NewReader(jbuf.Bytes()))
	if err != nil {
		return nil, err
	}
	var pbuf bytes.Buffer
	if err := png.Encode(&pbuf, rt); err != nil {
		return nil, err
	}
	return pbuf.Bytes(), nil
}

// jitterChannel applies contrast/brightness around mid-gray plus ±2 noise to a
// 16-bit color channel, returning the clamped 8-bit value.
func jitterChannel(v uint32, contrast, brightness float64, rng *rand.Rand) uint8 {
	f := float64(v >> 8)
	f = (f-128.0)*contrast + 128.0 + brightness + (rng.Float64()-0.5)*4.0
	if f < 0 {
		f = 0
	}
	if f > 255 {
		f = 255
	}
	return uint8(f + 0.5)
}

func clampInt(n, lo, hi int) int {
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
}
