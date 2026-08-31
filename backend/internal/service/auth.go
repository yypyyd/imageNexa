package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/repo"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

var (
	ErrAuthFailed              = errors.New("auth failed")
	ErrAdminNotInitialized     = errors.New("administrator is not initialized")
	ErrAdminAlreadyInitialized = errors.New("administrator is already initialized")
	ErrAuthServiceUnavailable  = errors.New("administrator authentication is not configured")
)

type AuthService struct {
	admins     *repo.AdminRepository
	sessions   *SessionService
	loginGuard *LoginGuard
}

// NewAdminAuthService is the typed constructor used by the 2API bootstrap.
func NewAdminAuthService(admins *repo.AdminRepository, sessions *SessionService, redisClient *redis.Client) *AuthService {
	service := &AuthService{admins: admins, sessions: sessions}
	if redisClient != nil {
		service.loginGuard = NewLoginGuard(redisClient)
	}
	return service
}

func (s *AuthService) Initialized(ctx context.Context) (bool, error) {
	if s == nil || s.admins == nil {
		return false, ErrAuthServiceUnavailable
	}
	return s.admins.Initialized(ctx)
}

func (s *AuthService) Initialize(
	ctx context.Context,
	username string,
	email string,
	password string,
	ip string,
) (*model.Admin, string, *SessionPayload, error) {
	if s == nil || s.admins == nil || s.sessions == nil {
		return nil, "", nil, ErrAuthServiceUnavailable
	}
	normalizedUsername, err := ValidateUsername(username)
	if err != nil {
		return nil, "", nil, err
	}
	normalizedEmail := ""
	if strings.TrimSpace(email) != "" {
		normalizedEmail, err = ValidateEmail(email)
		if err != nil {
			return nil, "", nil, err
		}
	}
	if err := ValidatePassword(password); err != nil {
		return nil, "", nil, err
	}
	passwordHash, err := GeneratePasswordHash(password)
	if err != nil {
		return nil, "", nil, err
	}
	now := time.Now()
	admin := &model.Admin{
		ID:             model.AdminSingletonID,
		SingletonKey:   1,
		Username:       normalizedUsername,
		Email:          normalizedEmail,
		PasswordHash:   "bcrypt$" + passwordHash,
		Status:         model.AdminStatusActive,
		SessionVersion: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := s.admins.Initialize(ctx, admin); err != nil {
		if errors.Is(err, repo.ErrAdminAlreadyInitialized) || errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil, "", nil, ErrAdminAlreadyInitialized
		}
		return nil, "", nil, err
	}
	if err := s.admins.TouchLogin(ctx, admin.ID, ip); err != nil {
		return nil, "", nil, err
	}
	token, session, err := s.sessions.Create(ctx, admin.ID, admin.SessionVersion)
	if err != nil {
		return nil, "", nil, err
	}
	admin.LastLoginIP = strings.TrimSpace(ip)
	admin.LastLoginAt = &now
	return admin, token, session, nil
}

func (s *AuthService) Login(ctx context.Context, identifier, password, ip string) (*model.Admin, string, *SessionPayload, error) {
	if s == nil || s.admins == nil || s.sessions == nil {
		return nil, "", nil, ErrAuthServiceUnavailable
	}
	normalizedIdentifier, err := ValidateLoginIdentifier(identifier)
	if err != nil {
		return nil, "", nil, err
	}
	if strings.TrimSpace(password) == "" {
		return nil, "", nil, errors.New("密码不能为空")
	}
	if s.loginGuard != nil {
		if err := s.loginGuard.Check(ctx, ip, normalizedIdentifier); err != nil {
			return nil, "", nil, err
		}
	}

	admin, err := s.admins.GetByIdentifier(ctx, normalizedIdentifier)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			s.recordLoginFailure(ctx, ip, normalizedIdentifier)
			return nil, "", nil, ErrAuthFailed
		}
		return nil, "", nil, err
	}
	if !admin.IsActive() || !VerifyPassword(password, admin.PasswordHash) {
		s.recordLoginFailure(ctx, ip, normalizedIdentifier)
		return nil, "", nil, ErrAuthFailed
	}
	if s.loginGuard != nil {
		if err := s.loginGuard.RecordSuccess(ctx, ip, normalizedIdentifier); err != nil {
			return nil, "", nil, err
		}
	}
	if err := s.admins.TouchLogin(ctx, admin.ID, ip); err != nil {
		return nil, "", nil, err
	}
	token, session, err := s.sessions.Create(ctx, admin.ID, admin.SessionVersion)
	if err != nil {
		return nil, "", nil, err
	}
	now := time.Now()
	admin.LastLoginAt = &now
	admin.LastLoginIP = strings.TrimSpace(ip)
	return admin, token, session, nil
}

func (s *AuthService) recordLoginFailure(ctx context.Context, ip, identifier string) {
	if s.loginGuard != nil {
		_ = s.loginGuard.RecordFailure(ctx, ip, identifier)
	}
}

// CurrentAdminFromCookie intentionally accepts only the opaque HttpOnly cookie
// value. An Authorization header can never authenticate the control plane.
func (s *AuthService) CurrentAdminFromCookie(ctx context.Context, cookieToken string) (*model.Admin, *SessionPayload, error) {
	if strings.TrimSpace(cookieToken) == "" {
		return nil, nil, nil
	}
	if s == nil || s.admins == nil || s.sessions == nil {
		return nil, nil, ErrAuthServiceUnavailable
	}
	payload, err := s.sessions.Validate(ctx, cookieToken)
	if err != nil || payload == nil {
		return nil, payload, err
	}
	admin, err := s.admins.GetByID(ctx, payload.AdminID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			_ = s.sessions.Destroy(ctx, cookieToken)
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if !admin.IsActive() || admin.SessionVersion != payload.SessionVersion {
		_ = s.sessions.Destroy(ctx, cookieToken)
		return nil, nil, nil
	}
	return admin, payload, nil
}

func (s *AuthService) Logout(ctx context.Context, cookieToken string) error {
	if s == nil || s.sessions == nil {
		return nil
	}
	return s.sessions.Destroy(ctx, cookieToken)
}

func (s *AuthService) ChangePassword(ctx context.Context, adminID, currentPassword, newPassword string) error {
	if s == nil || s.admins == nil {
		return ErrAuthServiceUnavailable
	}
	if strings.TrimSpace(currentPassword) == "" {
		return errors.New("当前密码不能为空")
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	admin, err := s.admins.GetByID(ctx, adminID)
	if err != nil {
		return err
	}
	if !VerifyPassword(currentPassword, admin.PasswordHash) {
		return errors.New("当前密码错误")
	}
	passwordHash, err := GeneratePasswordHash(newPassword)
	if err != nil {
		return err
	}
	return s.admins.UpdatePassword(ctx, admin.ID, "bcrypt$"+passwordHash)
}

func (s *AuthService) AuthConfig(ctx context.Context) (map[string]any, error) {
	initialized, err := s.Initialized(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"initialized":         initialized,
		"has_admin":           initialized,
		"initialization_open": !initialized,
		"server_time":         time.Now().Unix(),
	}, nil
}

func PublicAdmin(admin *model.Admin) map[string]any {
	if admin == nil {
		return nil
	}
	return map[string]any{
		"id":            admin.ID,
		"username":      admin.Username,
		"email":         admin.Email,
		"role":          "admin",
		"status":        admin.Status,
		"last_login_at": admin.LastLoginAt,
	}
}

// ParseBearer implements the OpenAI Authorization header shape. It rejects
// alternate schemes, missing values and extra whitespace-separated fields.
func ParseBearer(header string) string {
	fields := strings.Fields(strings.TrimSpace(header))
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return ""
	}
	return fields[1]
}

func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// V1 still reads boolean feature flags from the retained settings store.
func parseBoolSetting(value string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}
