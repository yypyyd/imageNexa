package service

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	"image/png"
	"sort"
	"strings"
	"sync"

	"github.com/esimov/pigo/core"
	xdraw "golang.org/x/image/draw"
)

// facefinder is Pigo's compact binary cascade. Keeping it in the binary makes
// the preprocessing deterministic in Docker and avoids a runtime model download.
//
//go:embed cascade/facefinder
var facefinder []byte

var faceDetector struct {
	sync.Once
	classifier *pigo.Pigo
	err        error
}

type detectedFace struct {
	box   image.Rectangle
	score float32
}

// applyReferenceFaceSwap covers the detected face region of the first face
// found by Pigo with a light red-silk mesh. No external face is ever introduced.
// References without a reliable face remain unchanged.
func applyReferenceFaceSwap(inputs []string) ([]string, error) {
	out := make([]string, 0, len(inputs))
	for _, raw := range inputs {
		data, err := decodeReferenceImage(raw)
		if err != nil {
			return nil, err
		}
		src, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%w: reference image is not decodable", ErrUnsupportedParams)
		}
		faces, err := detectFaces(src)
		if err != nil {
			if strings.Contains(err.Error(), "no face") {
				out = append(out, raw)
				continue
			}
			return nil, fmt.Errorf("%w: %v", ErrUnsupportedParams, err)
		}
		composite, err := veilFaceWithinImage(src, faces[0])
		if err != nil {
			return nil, err
		}
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, composite); err != nil {
			return nil, fmt.Errorf("%w: encode face veil: %v", ErrUnsupportedParams, err)
		}
		if encoded.Len() > maxReferenceImageBytes {
			return nil, ErrReferenceTooLarge
		}
		out = append(out, base64.StdEncoding.EncodeToString(encoded.Bytes()))
	}
	return out, nil
}

// veilFaceWithinImage keeps the surrounding composition intact and lays a
// light red-silk mesh only over the detected face region.
func veilFaceWithinImage(src image.Image, face detectedFace) (*image.RGBA, error) {
	bounds := src.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || int64(bounds.Dx())*int64(bounds.Dy()) > maxReferencePixels {
		return nil, ErrReferenceTooLarge
	}
	box := expandFacePanelBox(face.box.Intersect(image.Rect(0, 0, bounds.Dx(), bounds.Dy())), image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	if box.Dx() < 4 || box.Dy() < 2 {
		return nil, fmt.Errorf("%w: detected face is too small", ErrUnsupportedParams)
	}
	dst := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(dst, dst.Bounds(), src, bounds.Min, draw.Src)
	drawFaceVeil(dst, box)
	return dst, nil
}

// drawFaceVeil weaves red strands across the box in both directions. The mesh
// keeps a moderate density — thin strands with a medium gap — so the face keeps
// its likeness while the grid stays visible to the upstream model.
func drawFaceVeil(dst *image.RGBA, box image.Rectangle) {
	box = box.Intersect(dst.Bounds())
	if box.Dx() <= 0 || box.Dy() <= 0 {
		return
	}
	red := image.NewUniform(color.RGBA{R: 255, A: 255})
	strand := max(1, min(box.Dx(), box.Dy())/120)
	gap := max(5, min(box.Dx(), box.Dy())/22)
	for y := box.Min.Y; y < box.Max.Y; y += strand + gap {
		draw.Draw(dst, image.Rect(box.Min.X, y, box.Max.X, min(box.Max.Y, y+strand)), red, image.Point{}, draw.Src)
	}
	for x := box.Min.X; x < box.Max.X; x += strand + gap {
		draw.Draw(dst, image.Rect(x, box.Min.Y, min(box.Max.X, x+strand), box.Max.Y), red, image.Point{}, draw.Src)
	}
}

// expandFacePanelBox keeps the mask inside the Pigo rectangle. Pigo's square
// includes a little hair and ear padding, so trim those edges before veiling.
func expandFacePanelBox(box, bounds image.Rectangle) image.Rectangle {
	box = box.Intersect(bounds)
	if box.Dx() <= 0 || box.Dy() <= 0 {
		return box
	}
	padX := max(1, int(float64(box.Dx())*0.08))
	padTop := max(1, int(float64(box.Dy())*0.14))
	padBottom := max(1, int(float64(box.Dy())*0.06))
	return image.Rect(
		box.Min.X+padX,
		box.Min.Y+padTop,
		box.Max.X-padX,
		box.Max.Y-padBottom,
	)
}

func decodeReferenceImage(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if comma := bytes.IndexByte([]byte(value), ','); comma >= 0 {
		value = strings.TrimSpace(value[comma+1:])
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("%w: invalid face image encoding", ErrUnsupportedParams)
	}
	if len(data) > maxReferenceImageBytes {
		return nil, ErrReferenceTooLarge
	}
	return data, nil
}

func detectFaces(src image.Image) ([]detectedFace, error) {
	faces, primaryErr := runPigoFaces(src)
	faces = filterFaceCandidates(faces, src.Bounds())
	if len(faces) >= 2 {
		return faces, nil
	}
	// AI artwork often cuts a face at a panel edge. Run Pigo on a few enlarged
	// vertical windows, including a mirrored edge pad, so the cascade sees a
	// complete candidate without changing the original pixels we crop from.
	windowFaces, windowErr := detectFacesInWindows(src)
	faces = filterFaceCandidates(mergeDetectedFaces(append(faces, windowFaces...)), src.Bounds())
	if len(faces) > 0 {
		return faces, nil
	}
	if primaryErr != nil {
		return nil, primaryErr
	}
	return nil, windowErr
}

func filterFaceCandidates(faces []detectedFace, bounds image.Rectangle) []detectedFace {
	minSide := max(24, int(float64(min(bounds.Dx(), bounds.Dy()))*0.18))
	out := make([]detectedFace, 0, len(faces))
	for _, face := range faces {
		if min(face.box.Dx(), face.box.Dy()) >= minSide {
			out = append(out, face)
		}
	}
	return out
}

func runPigoFaces(src image.Image) ([]detectedFace, error) {
	faceDetector.Do(func() {
		faceDetector.classifier, faceDetector.err = pigo.NewPigo().Unpack(facefinder)
	})
	if faceDetector.err != nil {
		return nil, faceDetector.err
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("image has no pixels")
	}
	minSize := max(20, min(width, height)/8)
	maxSize := min(width, height)
	dets := faceDetector.classifier.RunCascade(pigo.CascadeParams{
		ImageParams: pigo.ImageParams{
			Pixels: pigo.RgbToGrayscale(src),
			Rows:   height,
			Cols:   width,
			Dim:    width,
		},
		MinSize:     minSize,
		MaxSize:     maxSize,
		ShiftFactor: 0.1,
		ScaleFactor: 1.1,
	}, 0.0)
	if len(dets) == 0 {
		return nil, fmt.Errorf("no face detected")
	}
	dets = faceDetector.classifier.ClusterDetections(dets, 0.2)
	faces := make([]detectedFace, 0, len(dets))
	for _, det := range dets {
		half := det.Scale / 2
		left := max(0, det.Col-half)
		top := max(0, det.Row-half)
		right := min(width, det.Col+det.Scale-half)
		bottom := min(height, det.Row+det.Scale-half)
		if right-left >= 2 && bottom-top >= 2 {
			faces = append(faces, detectedFace{box: image.Rect(left, top, right, bottom), score: det.Q})
		}
	}
	if len(faces) == 0 {
		return nil, fmt.Errorf("no usable face detected")
	}
	sort.Slice(faces, func(i, j int) bool { return faces[i].box.Dx()*faces[i].box.Dy() > faces[j].box.Dx()*faces[j].box.Dy() })
	return faces, nil
}

func detectFacesInWindows(src image.Image) ([]detectedFace, error) {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	windowW := max(180, int(float64(width)*0.43))
	windowH := max(240, int(float64(height)*0.68))
	windowW = min(width, windowW)
	windowH = min(height, windowH)
	if windowW < 20 || windowH < 20 {
		return nil, fmt.Errorf("no face detected")
	}
	starts := []int{0, int(float64(width) * 0.15), max(0, width-windowW)}
	var found []detectedFace
	for _, start := range starts {
		if start+windowW > width {
			start = width - windowW
		}
		cropRect := image.Rect(start, 0, start+windowW, windowH)
		crop := image.NewRGBA(image.Rect(0, 0, cropRect.Dx(), cropRect.Dy()))
		draw.Draw(crop, crop.Bounds(), src, cropRect.Min, draw.Src)
		found = append(found, remapWindowFaces(crop, start, 0, 4, runScaledPigo(crop, false))...)

		// Mirror only the visible part near the right edge. This lets Pigo
		// classify a half-face panel, but the resulting crop is still taken from
		// the original image coordinates.
		if cropRect.Max.X == width {
			axis := max(20, int(float64(crop.Bounds().Dx())*0.47))
			pad := mirrorPad(crop, axis)
			found = append(found, remapWindowFaces(pad, start, 0, 4, runScaledPigo(pad, false))...)
		}
	}
	found = mergeDetectedFaces(found)
	if len(found) == 0 {
		return nil, fmt.Errorf("no face detected")
	}
	return found, nil
}

func runScaledPigo(src image.Image, _ bool) []detectedFace {
	const scale = 4
	big := image.NewRGBA(image.Rect(0, 0, src.Bounds().Dx()*scale, src.Bounds().Dy()*scale))
	xdraw.CatmullRom.Scale(big, big.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	faces, _ := runPigoFaces(big)
	return faces
}

func remapWindowFaces(crop image.Image, offsetX, offsetY, scale int, faces []detectedFace) []detectedFace {
	out := make([]detectedFace, 0, len(faces))
	for _, face := range faces {
		cx := float64(face.box.Min.X+face.box.Max.X) / 2 / float64(scale)
		cy := float64(face.box.Min.Y+face.box.Max.Y) / 2 / float64(scale)
		size := float64(max(face.box.Dx(), face.box.Dy())) / float64(scale)
		left := max(0, int(cx-size/2)+offsetX)
		top := max(0, int(cy-size/2)+offsetY)
		right := min(offsetX+crop.Bounds().Dx(), int(cx+size/2)+offsetX)
		bottom := min(offsetY+crop.Bounds().Dy(), int(cy+size/2)+offsetY)
		if right-left >= 2 && bottom-top >= 2 {
			out = append(out, detectedFace{box: image.Rect(left, top, right, bottom), score: face.score})
		}
	}
	return out
}

func mirrorPad(src image.Image, axis int) image.Image {
	axis = min(src.Bounds().Dx(), max(1, axis))
	dst := image.NewRGBA(image.Rect(0, 0, axis*2, src.Bounds().Dy()))
	for y := 0; y < dst.Bounds().Dy(); y++ {
		for x := 0; x < axis; x++ {
			dst.Set(x, y, src.At(x, y))
		}
		for x := axis; x < axis*2; x++ {
			dst.Set(x, y, src.At(max(0, min(src.Bounds().Dx()-1, 2*axis-1-x)), y))
		}
	}
	return dst
}

func mergeDetectedFaces(faces []detectedFace) []detectedFace {
	merged := make([]detectedFace, 0, len(faces))
	for _, candidate := range faces {
		duplicate := false
		for _, existing := range merged {
			if faceIoU(candidate.box, existing.box) > 0.35 {
				duplicate = true
				break
			}
		}
		if !duplicate {
			merged = append(merged, candidate)
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].box.Dx()*merged[i].box.Dy() > merged[j].box.Dx()*merged[j].box.Dy()
	})
	return merged
}

func faceIoU(a, b image.Rectangle) float64 {
	intersection := a.Intersect(b)
	if intersection.Empty() {
		return 0
	}
	area := float64(a.Dx()*a.Dy() + b.Dx()*b.Dy() - intersection.Dx()*intersection.Dy())
	if area <= 0 {
		return 0
	}
	return float64(intersection.Dx()*intersection.Dy()) / area
}
