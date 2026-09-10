package service

import (
	"context"
	"strings"
)

const (
	maxReferencePixels = 100_000_000
)

// facePanelModels are the Adobe and Oreate Seedance routes that accept image
// references and receive the local Pigo face-panel transform.
var facePanelModels = map[string]bool{
	"firefly-seedance-2":       true,
	"firefly-seedance-2-fast":  true,
	"adobe-seedance-2.0":       true,
	"adobe-seedance-2.0-fast":  true,
	"seedance20":               true,
	"seedance20-fast":          true,
	"sd2.0":                    true,
	"sd2.0-fast":               true,
	"oreate-seedance-1.5-pro":  true,
	"oreate-seedance-2.0-mini": true,
	"oreate-seedance-2.0-fast": true,
	"oreate-seedance-2.0":      true,
	"oreate-seedance-2.5":      true,
}

// facePanelTransformEnabled 是人脸遮罩网格的总开关：置为 false 时所有模型都收到
// 未改动的原始参考图。
const facePanelTransformEnabled = true

// shouldApplyReferenceGrid is retained as the wire-compatible name for the
// reference_grid flag. Models outside facePanelModels receive the original
// reference bytes unchanged, even when the legacy flag is present.
func (s *V1Service) shouldApplyReferenceGrid(_ context.Context, modelID string, _ bool) bool {
	if !facePanelTransformEnabled {
		return false
	}
	return facePanelModels[strings.ToLower(strings.TrimSpace(modelID))]
}
