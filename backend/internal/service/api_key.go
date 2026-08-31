package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
	"gorm.io/gorm"
)

var (
	ErrInvalidAPICredential         = errors.New("invalid api key")
	ErrCredentialServiceUnavailable = errors.New("api credential service is not configured")
)

type APICredentialService struct {
	credentials *repo.APICredentialRepository
	concurrency *ConcurrencyService
}

func NewAPICredentialService(credentials *repo.APICredentialRepository) *APICredentialService {
	return &APICredentialService{credentials: credentials}
}

func (s *APICredentialService) SetConcurrency(concurrency *ConcurrencyService) {
	if s != nil {
		s.concurrency = concurrency
	}
}

func (s *APICredentialService) ActiveRequests(ctx context.Context, credentialID string) int64 {
	if s == nil || s.concurrency == nil {
		return 0
	}
	return s.concurrency.ActiveCount(ctx, "conc:k:"+credentialID)
}

type CreateAPICredentialInput struct {
	Name             string `json:"name"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
}

type UpdateAPICredentialInput struct {
	Name             *string `json:"name"`
	Status           *string `json:"status"`
	ConcurrencyLimit *int    `json:"concurrency_limit"`
}

type CreatedAPICredential struct {
	Credential model.APICredential `json:"credential"`
	Key        string              `json:"key"`
}

func (s *APICredentialService) List(ctx context.Context, includeRevoked bool) ([]model.APICredential, error) {
	if s == nil || s.credentials == nil {
		return nil, ErrCredentialServiceUnavailable
	}
	return s.credentials.List(ctx, includeRevoked)
}

func (s *APICredentialService) Create(ctx context.Context, input CreateAPICredentialInput) (*CreatedAPICredential, error) {
	if s == nil || s.credentials == nil {
		return nil, ErrCredentialServiceUnavailable
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, errors.New("API Key 名称不能为空")
	}
	if len(name) > 100 {
		return nil, errors.New("API Key 名称不能超过 100 个字符")
	}
	if input.ConcurrencyLimit < 0 {
		return nil, errors.New("并发上限不能小于 0")
	}
	plain, err := generatePlainAPIKey()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	credentialID, err := randomSecret(9)
	if err != nil {
		return nil, err
	}
	credential := model.APICredential{
		ID:               "cred-" + credentialID,
		Name:             name,
		KeyPreview:       previewAPIKey(plain),
		KeyHash:          HashAPIKey(plain),
		Status:           model.APICredentialStatusActive,
		ConcurrencyLimit: input.ConcurrencyLimit,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if err := s.credentials.Create(ctx, &credential); err != nil {
		return nil, err
	}
	return &CreatedAPICredential{Credential: credential, Key: plain}, nil
}

func (s *APICredentialService) Update(
	ctx context.Context,
	credentialID string,
	input UpdateAPICredentialInput,
) (*model.APICredential, error) {
	if s == nil || s.credentials == nil {
		return nil, ErrCredentialServiceUnavailable
	}
	existing, err := s.credentials.GetByID(ctx, credentialID)
	if err != nil {
		return nil, err
	}
	if existing.RevokedAt != nil || existing.Status == model.APICredentialStatusRevoked {
		return nil, errors.New("已吊销的 API Key 不能修改")
	}
	patch := make(map[string]any)
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 100 {
			return nil, errors.New("API Key 名称需为 1 到 100 个字符")
		}
		patch["name"] = name
	}
	if input.ConcurrencyLimit != nil {
		if *input.ConcurrencyLimit < 0 {
			return nil, errors.New("并发上限不能小于 0")
		}
		patch["concurrency_limit"] = *input.ConcurrencyLimit
	}
	if input.Status != nil {
		status := strings.ToLower(strings.TrimSpace(*input.Status))
		switch status {
		case model.APICredentialStatusActive, model.APICredentialStatusDisabled:
			patch["status"] = status
		case model.APICredentialStatusRevoked:
			return nil, errors.New("请使用吊销接口永久吊销 API Key")
		default:
			return nil, errors.New("API Key 状态不正确")
		}
	}
	if len(patch) == 0 {
		return existing, nil
	}
	patch["updated_at"] = time.Now()
	credential, err := s.credentials.Update(ctx, credentialID, patch)
	if err != nil {
		return nil, err
	}
	return credential, nil
}

func (s *APICredentialService) Revoke(ctx context.Context, credentialID string) error {
	if s == nil || s.credentials == nil {
		return ErrCredentialServiceUnavailable
	}
	return s.credentials.Revoke(ctx, credentialID)
}

func (s *APICredentialService) Rotate(ctx context.Context, credentialID string) (*CreatedAPICredential, error) {
	if s == nil || s.credentials == nil {
		return nil, ErrCredentialServiceUnavailable
	}
	existing, err := s.credentials.GetByID(ctx, credentialID)
	if err != nil {
		return nil, err
	}
	plain, err := generatePlainAPIKey()
	if err != nil {
		return nil, err
	}
	id, err := randomSecret(9)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	replacement := model.APICredential{
		ID: "cred-" + id, Name: existing.Name, KeyPreview: previewAPIKey(plain), KeyHash: HashAPIKey(plain),
		Status: existing.Status, ConcurrencyLimit: existing.ConcurrencyLimit,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.credentials.Rotate(ctx, credentialID, &replacement); err != nil {
		return nil, err
	}
	return &CreatedAPICredential{Credential: replacement, Key: plain}, nil
}

// AuthenticateBearer accepts only OpenAI's Authorization: Bearer sk-* shape.
// It deliberately ignores x-api-key, cookies and query parameters.
func (s *APICredentialService) AuthenticateBearer(ctx context.Context, authorization string) (*model.APICredential, error) {
	plain := ParseBearer(authorization)
	if !validPlainAPIKey(plain) {
		return nil, ErrInvalidAPICredential
	}
	if s == nil || s.credentials == nil {
		return nil, ErrCredentialServiceUnavailable
	}
	credential, err := s.credentials.GetActiveByHash(ctx, HashAPIKey(plain))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvalidAPICredential
		}
		return nil, err
	}
	_ = s.credentials.TouchLastUsed(ctx, credential.ID, time.Now())
	return credential, nil
}

func generatePlainAPIKey() (string, error) {
	secret, err := randomSecret(36)
	if err != nil {
		return "", err
	}
	return "sk-" + secret, nil
}

func validPlainAPIKey(plain string) bool {
	if len(plain) < 16 || len(plain) > 128 || !strings.HasPrefix(plain, "sk-") {
		return false
	}
	for _, char := range plain[3:] {
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func previewAPIKey(plain string) string {
	if len(plain) <= 8 {
		return "sk-••••"
	}
	return plain[:7] + "…" + plain[len(plain)-4:]
}
