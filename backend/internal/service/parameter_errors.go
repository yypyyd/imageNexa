package service

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"backend/internal/model"
)

// ParameterError is constructed only from local validation and catalog values.
// Never include a supplied prompt, credential, URL, or raw provider error.
type ParameterError struct {
	Param     string
	Message   string
	Supported []string
}

func (e *ParameterError) Error() string { return e.Message }
func (e *ParameterError) Unwrap() error { return ErrUnsupportedParams }

func invalidParameter(param, message string, supported ...string) error {
	return &ParameterError{Param: param, Message: message, Supported: supported}
}

func (s *V1Service) unsupportedRouteParameters(ctx context.Context, logicalID string, req model.RouteRequirements) error {
	routes, err := s.models.Routes().ListRoutes(ctx, logicalID, true)
	if err != nil {
		return err
	}
	return describeUnsupportedRoute(routes, req)
}

func describeUnsupportedRoute(routes []model.ModelRoute, req model.RouteRequirements) error {
	var profiles []model.CapabilityProfile
	for _, route := range routes {
		profiles = append(profiles, model.DecodeCapabilityProfiles(route.Capabilities)...)
	}
	fields := []struct {
		name, wanted string
		values       func(model.CapabilityProfile) []string
	}{
		{"operation", req.Operation, func(p model.CapabilityProfile) []string { return p.Operations }},
		{"aspect_ratio", req.Ratio, func(p model.CapabilityProfile) []string { return p.Ratios }},
		{"resolution", req.Resolution, func(p model.CapabilityProfile) []string { return p.Resolutions }},
		{"duration", req.Duration, func(p model.CapabilityProfile) []string { return p.Durations }},
	}
	// Narrow profiles after each field, preserving valid combinations rather
	// than advertising a cross-product of unrelated provider capabilities.
	for _, field := range fields {
		if strings.TrimSpace(field.wanted) == "" {
			continue
		}
		allowed := map[string]bool{}
		var matched []model.CapabilityProfile
		for _, profile := range profiles {
			for _, value := range field.values(profile) {
				allowed[value] = true
				if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(field.wanted)) {
					matched = append(matched, profile)
				}
			}
		}
		if len(matched) == 0 && len(allowed) > 0 {
			values := make([]string, 0, len(allowed))
			for value := range allowed {
				values = append(values, value)
			}
			sort.Strings(values)
			return invalidParameter(field.name, fmt.Sprintf("Unsupported %s for this parameter combination. Supported values: %s.", field.name, strings.Join(values, ", ")), values...)
		}
		profiles = matched
	}
	maxImages, maxVideos, maxAudios := 0, 0, 0
	for _, p := range profiles {
		maxImages = max(maxImages, p.MaxReferenceImages)
		maxVideos = max(maxVideos, p.MaxReferenceVideos)
		maxAudios = max(maxAudios, p.MaxReferenceAudios)
	}
	for _, limit := range []struct {
		name           string
		count, maximum int
	}{{"reference_images", req.ReferenceImages, maxImages}, {"reference_videos", req.ReferenceVideos, maxVideos}, {"reference_audios", req.ReferenceAudios, maxAudios}} {
		if limit.count > limit.maximum && len(profiles) > 0 {
			return invalidParameter(limit.name, fmt.Sprintf("%s supports at most %d item(s) for this parameter combination.", limit.name, limit.maximum))
		}
	}
	return invalidParameter("parameters", "This parameter combination is not supported. Check the model's capability profiles.")
}
