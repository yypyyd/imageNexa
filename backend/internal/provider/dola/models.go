package dola

import (
	"fmt"
	"strings"
)

// DefaultVideoModel is the ability_param model id Dola's web picker currently
// submits for the "Dreamina Seedance 2.5" option. The route's upstream model
// overrides this when it carries an explicit value.
const DefaultVideoModel = "seedance_v2.5"

func NormalizeModelID(modelID string) string {
	modelID = strings.ToLower(strings.TrimSpace(modelID))
	if modelID == "" {
		return DefaultVideoModel
	}
	return modelID
}

// RequiredCredits is retained for adapter compatibility. The scheduler unit is
// generations: only a 30-second video is supported, and each costs one use.
func RequiredCredits(duration int) (int, error) {
	if duration != 30 {
		return 0, fmt.Errorf("dola: only 30-second videos are supported")
	}
	return 1, nil
}
