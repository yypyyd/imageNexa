package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gorm.io/datatypes"
)

// These coercion helpers are shared by retained provider-account and V1 code.
// They live outside the removed user/admin product services so the data plane
// does not depend on unreachable legacy modules.
func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

func floatValue(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case json.Number:
		result, _ := typed.Float64()
		return result
	case string:
		result, _ := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return result
	default:
		return 0
	}
}

func intValue(value any) int {
	return int(floatValue(value))
}

func boolValueWithDefault(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	if result, ok := value.(bool); ok {
		return result
	}
	if text, ok := value.(string); ok {
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
	}
	return fallback
}

func jsonArray(value any) datatypes.JSON {
	if value == nil {
		return datatypes.JSON([]byte("[]"))
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return datatypes.JSON([]byte("[]"))
	}
	return datatypes.JSON(encoded)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
