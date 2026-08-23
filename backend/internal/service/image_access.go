package service

import (
	"errors"
	"strings"

	"backend/internal/repo"
)

type ImageAccessService struct {
	generatedRoot string
	showcase      *repo.ShowcaseRepository
	auth          *AuthService
}

func NewImageAccessService(generatedRoot string, showcase *repo.ShowcaseRepository, auth *AuthService) *ImageAccessService {
	return &ImageAccessService{
		generatedRoot: generatedRoot,
		showcase:      showcase,
		auth:          auth,
	}
}

// Resolve validates the path params and returns the object key (user/name).
// Existence isn't checked here — that's the storage GET's job (404 if missing).
func (s *ImageAccessService) Resolve(user, name string) (string, error) {
	user = strings.TrimSpace(user)
	name = strings.TrimSpace(name)
	if user == "" || name == "" {
		return "", errors.New("missing path params")
	}
	// :user and :name are single path segments (gin won't match "/"); guard
	// against traversal tokens anyway.
	if strings.Contains(user, "..") || strings.Contains(name, "..") ||
		strings.ContainsAny(user, `/\`) || strings.ContainsAny(name, `/\`) {
		return "", errors.New("invalid image path")
	}
	return user + "/" + name, nil
}
