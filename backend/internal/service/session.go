package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type SessionPayload struct {
	AdminID        string `json:"admin_id"`
	SessionVersion int64  `json:"session_version"`
	CSRFToken      string `json:"csrf_token"`
	CreatedAt      int64  `json:"created_at"`
	ExpiresAt      int64  `json:"expires_at"`
}

type SessionService struct {
	client     *redis.Client
	prefix     string
	ttl        time.Duration
	slideAfter time.Duration
	slideTo    time.Duration
}

func NewSessionService(client *redis.Client, ttl, slideAfter time.Duration) *SessionService {
	return &SessionService{
		client:     client,
		prefix:     "admin_session:",
		ttl:        ttl,
		slideAfter: slideAfter,
		slideTo:    ttl,
	}
}

func (s *SessionService) Create(ctx context.Context, adminID string, version ...int64) (string, *SessionPayload, error) {
	if s == nil || s.client == nil {
		return "", nil, errors.New("session store is not configured")
	}
	sessionVersion := int64(1)
	if len(version) > 0 && version[0] > 0 {
		sessionVersion = version[0]
	}
	now := time.Now()
	token, err := randomSecret(36)
	if err != nil {
		return "", nil, err
	}
	csrfToken, err := randomSecret(32)
	if err != nil {
		return "", nil, err
	}
	payload := &SessionPayload{
		AdminID:        adminID,
		SessionVersion: sessionVersion,
		CSRFToken:      csrfToken,
		CreatedAt:      now.Unix(),
		ExpiresAt:      now.Add(s.ttl).Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	if err := s.client.Set(ctx, s.key(token), raw, s.ttl).Err(); err != nil {
		return "", nil, err
	}
	return token, payload, nil
}

func (s *SessionService) Validate(ctx context.Context, token string) (*SessionPayload, error) {
	if token == "" {
		return nil, nil
	}
	if s == nil || s.client == nil {
		return nil, errors.New("session store is not configured")
	}
	key := s.key(token)
	raw, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}

	var payload SessionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		_ = s.client.Del(ctx, key).Err()
		return nil, nil
	}
	if payload.AdminID == "" || payload.CSRFToken == "" || payload.SessionVersion < 1 || payload.ExpiresAt <= time.Now().Unix() {
		_ = s.client.Del(ctx, key).Err()
		return nil, nil
	}

	ttl, err := s.client.TTL(ctx, key).Result()
	if err == nil && ttl > 0 && ttl < s.slideAfter {
		renewed := payload
		renewed.ExpiresAt = time.Now().Add(s.slideTo).Unix()
		if updated, marshalErr := json.Marshal(&renewed); marshalErr == nil {
			if setErr := s.client.Set(ctx, key, updated, s.slideTo).Err(); setErr == nil {
				payload.ExpiresAt = renewed.ExpiresAt
			}
		}
	}
	return &payload, nil
}

func (s *SessionService) Destroy(ctx context.Context, token string) error {
	if token == "" || s == nil || s.client == nil {
		return nil
	}
	return s.client.Del(ctx, s.key(token)).Err()
}

func (s *SessionService) key(token string) string {
	return s.prefix + token
}

func randomSecret(byteLength int) (string, error) {
	if byteLength < 1 {
		return "", errors.New("secret length must be positive")
	}
	raw := make([]byte, byteLength)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
