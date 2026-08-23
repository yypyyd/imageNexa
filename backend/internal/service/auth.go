package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"

	"backend/internal/model"
	"backend/internal/repo"

	"gorm.io/gorm"
)

var ErrAuthFailed = errors.New("auth failed")

type AuthService struct {
	users      *repo.UserRepository
	settings   *repo.SiteSettingRepository
	sessions   *SessionService
	codes      *EmailCodeService
	smtp       *SMTPService
	loginGuard *LoginGuard
	cgroups    *repo.ConcurrencyGroupRepository
}

type AuthSettings struct {
	Open               bool
	EmailCode          bool
	AllowPasswordReset bool
	AllowedDomains     []string
}

func NewAuthService(
	users *repo.UserRepository,
	settings *repo.SiteSettingRepository,
	sessions *SessionService,
	codes *EmailCodeService,
	smtp *SMTPService,
	cgroups *repo.ConcurrencyGroupRepository,
) *AuthService {
	return &AuthService{
		users:      users,
		settings:   settings,
		sessions:   sessions,
		codes:      codes,
		smtp:       smtp,
		loginGuard: NewLoginGuard(codes.Redis()),
		cgroups:    cgroups,
	}
}

func (s *AuthService) CurrentUserFromBearer(ctx context.Context, authHeader string) (*model.User, *SessionPayload, error) {
	token := ParseBearer(authHeader)
	return s.currentUserFromToken(ctx, token)
}

func (s *AuthService) CurrentUserFromRequest(ctx context.Context, authHeader, cookieToken string) (*model.User, *SessionPayload, error) {
	if user, session, err := s.CurrentUserFromBearer(ctx, authHeader); err != nil || user != nil || session != nil {
		return user, session, err
	}
	return s.currentUserFromToken(ctx, cookieToken)
}

func (s *AuthService) currentUserFromToken(ctx context.Context, token string) (*model.User, *SessionPayload, error) {
	if token == "" {
		return nil, nil, nil
	}

	payload, err := s.sessions.Validate(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if payload == nil {
		return nil, nil, nil
	}

	user, err := s.users.GetByID(ctx, payload.UserID)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	if user.Status != "active" {
		return nil, nil, nil
	}
	return user, payload, nil
}

func (s *AuthService) Login(ctx context.Context, identifier, password, ip string) (*model.User, string, *SessionPayload, error) {
	normalizedIdentifier, err := ValidateLoginIdentifier(identifier)
	if err != nil {
		return nil, "", nil, err
	}
	if strings.TrimSpace(password) == "" {
		return nil, "", nil, errors.New("密码不能为空")
	}

	// Exponential-backoff lockout per (ip, account) + per-ip spray (Python
	// api/auth.py:226-237 via core/login_guard.py).
	if err := s.loginGuard.Check(ctx, ip, normalizedIdentifier); err != nil {
		return nil, "", nil, err
	}

	user, err := s.users.GetByIdentifier(ctx, normalizedIdentifier)
	if err != nil {
		if err == gorm.ErrRecordNotFound {
			if rerr := s.loginGuard.RecordFailure(ctx, ip, normalizedIdentifier); rerr != nil {
				return nil, "", nil, rerr
			}
			return nil, "", nil, ErrAuthFailed
		}
		return nil, "", nil, err
	}
	if user.Status != "active" || !VerifyPassword(password, user.PasswordHash) {
		if rerr := s.loginGuard.RecordFailure(ctx, ip, normalizedIdentifier); rerr != nil {
			return nil, "", nil, rerr
		}
		return nil, "", nil, ErrAuthFailed
	}
	if err := s.loginGuard.RecordSuccess(ctx, ip, normalizedIdentifier); err != nil {
		return nil, "", nil, err
	}

	if err := s.users.TouchLogin(ctx, user.ID, ip); err != nil {
		return nil, "", nil, err
	}
	token, payload, err := s.sessions.Create(ctx, user.ID)
	if err != nil {
		return nil, "", nil, err
	}
	return user, token, payload, nil
}

func (s *AuthService) SendCode(ctx context.Context, email, purpose string) error {
	cfg, err := s.loadAuthSettings(ctx)
	if err != nil {
		return err
	}
	if !cfg.EmailCode {
		return errors.New("未开启邮箱验证码")
	}

	normalizedEmail, err := ValidateEmail(email)
	if err != nil {
		return err
	}
	purpose = strings.ToLower(strings.TrimSpace(purpose))
	switch purpose {
	case "register", "reset":
	default:
		return errors.New("验证码用途不正确")
	}

	if purpose == "register" && !EmailDomainAllowed(normalizedEmail, cfg.AllowedDomains) {
		return errors.New("该邮箱后缀不允许注册")
	}

	code, err := s.codes.Issue(ctx, normalizedEmail, purpose)
	if err != nil {
		return err
	}
	return s.smtp.SendCode(ctx, s.loadSMTPSettings(ctx), normalizedEmail, code, purpose)
}

func (s *AuthService) Register(ctx context.Context, email, username, password, inviteCode, emailCode, ip string) (*model.User, string, *SessionPayload, error) {
	normalizedEmail, err := ValidateEmail(email)
	if err != nil {
		return nil, "", nil, err
	}
	normalizedUsername, err := ValidateUsername(username)
	if err != nil {
		return nil, "", nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, "", nil, err
	}

	settings, err := s.loadAuthSettings(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	hasAdmin, err := s.users.HasAdmin(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	// The very first account ever bootstraps the admin and skips the open
	// toggle, the email-domain whitelist, and the email-code gate (Python
	// api/auth.py:195-204). All three are only enforced once an admin exists.
	if hasAdmin && !settings.Open {
		return nil, "", nil, errors.New("当前未开放注册")
	}
	if hasAdmin && !EmailDomainAllowed(normalizedEmail, settings.AllowedDomains) {
		return nil, "", nil, errors.New("该邮箱后缀不允许注册")
	}
	if hasAdmin && settings.EmailCode {
		ok, err := s.codes.Verify(ctx, normalizedEmail, "register", emailCode)
		if err != nil {
			return nil, "", nil, err
		}
		if !ok {
			return nil, "", nil, errors.New("邮箱验证码错误或已过期")
		}
	}

	exists, err := s.users.ExistsEmail(ctx, normalizedEmail, "")
	if err != nil {
		return nil, "", nil, err
	}
	if exists {
		return nil, "", nil, errors.New("邮箱已存在")
	}
	exists, err = s.users.ExistsName(ctx, normalizedUsername, "")
	if err != nil {
		return nil, "", nil, err
	}
	if exists {
		return nil, "", nil, errors.New("用户名已存在")
	}

	passwordHash, err := HashPassword(password)
	if err != nil {
		return nil, "", nil, err
	}
	role := "user"
	if !hasAdmin {
		role = "admin"
	}

	var invitedBy *string
	if strings.TrimSpace(inviteCode) != "" {
		inviter, err := s.users.GetByInviteCode(ctx, inviteCode)
		if err == nil {
			invitedBy = &inviter.ID
		}
	}

	now := time.Now()
	user := &model.User{
		ID:           "u-" + randomUpper(10),
		Email:        normalizedEmail,
		Name:         normalizedUsername,
		PasswordHash: passwordHash,
		Role:         role,
		Status:       "active",
		InviteCode:   randomInviteCode(),
		InvitedBy:    invitedBy,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	// Bind new users to the default concurrency group.
	if s.cgroups != nil {
		if def, derr := s.cgroups.GetDefault(ctx); derr == nil && def != nil {
			user.ConcurrencyGroupID = def.ID
		}
	}
	if err := s.users.Create(ctx, user); err != nil {
		return nil, "", nil, err
	}
	if err := s.users.TouchLogin(ctx, user.ID, ip); err != nil {
		return nil, "", nil, err
	}
	token, payload, err := s.sessions.Create(ctx, user.ID)
	if err != nil {
		return nil, "", nil, err
	}
	created, err := s.users.GetByID(ctx, user.ID)
	if err != nil {
		return nil, "", nil, err
	}
	return created, token, payload, nil
}

func (s *AuthService) ResetPassword(ctx context.Context, email, password, emailCode, ip string) error {
	settings, err := s.loadAuthSettings(ctx)
	if err != nil {
		return err
	}
	if !settings.EmailCode || !settings.AllowPasswordReset {
		return errors.New("未开放找回密码")
	}
	normalizedEmail, err := ValidateEmail(email)
	if err != nil {
		return err
	}
	if err := ValidatePassword(password); err != nil {
		return err
	}
	// Rate-limit reset attempts per IP+email so the 6-digit code can't be ground
	// down even with the single-use + wrong-guess cap (Python api/auth.py:257-268).
	guardID := "reset:" + normalizedEmail
	if err := s.loginGuard.Check(ctx, ip, guardID); err != nil {
		return err
	}
	ok, err := s.codes.Verify(ctx, normalizedEmail, "reset", emailCode)
	if err != nil {
		return err
	}
	if !ok {
		if rerr := s.loginGuard.RecordFailure(ctx, ip, guardID); rerr != nil {
			return rerr
		}
		return errors.New("邮箱验证码错误或已过期")
	}
	if err := s.loginGuard.RecordSuccess(ctx, ip, guardID); err != nil {
		return err
	}
	passwordHash, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.users.SetPasswordByEmail(ctx, normalizedEmail, passwordHash)
	return err
}

func (s *AuthService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if strings.TrimSpace(currentPassword) == "" {
		return errors.New("当前密码不能为空")
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if !VerifyPassword(currentPassword, user.PasswordHash) {
		return errors.New("当前密码错误")
	}
	passwordHash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	_, err = s.users.Update(ctx, userID, map[string]any{
		"password_hash": passwordHash,
	})
	return err
}

func (s *AuthService) Logout(ctx context.Context, token string) error {
	return s.sessions.Destroy(ctx, token)
}

func (s *AuthService) AuthConfig(ctx context.Context) (map[string]any, error) {
	hasAdmin, err := s.users.HasAdmin(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.loadAuthSettings(ctx)
	if err != nil {
		return nil, err
	}
	credits, err := s.loadCreditSettings(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"open":                  settings.Open,
		"email_code":            settings.EmailCode,
		"allow_password_reset":  settings.AllowPasswordReset,
		"allowed_email_domains": settings.AllowedDomains,
		"has_admin":             hasAdmin,
		"checkin_enabled":       credits.CheckinEnabled,
		"checkin_reward":        credits.CheckinReward,
		"invite_enabled":        credits.InviteEnabled,
		"invite_reward":         credits.InviteReward,
		"server_time":           time.Now().Unix(),
	}, nil
}

func (s *AuthService) PublicUser(ctx context.Context, user *model.User) (map[string]any, error) {
	if user == nil {
		return nil, nil
	}
	credits, err := s.loadCreditSettings(ctx)
	if err != nil {
		return nil, err
	}
	stats, err := s.users.InviteStats(ctx, user.ID, credits.InviteReward)
	if err != nil {
		return nil, err
	}
	// Concurrency group + its cap (0 = unlimited) for the profile page.
	concName, concMax := "", 0
	if s.cgroups != nil {
		var g *model.ConcurrencyGroup
		if user.ConcurrencyGroupID != "" {
			g, _ = s.cgroups.Get(ctx, user.ConcurrencyGroupID)
		}
		if g == nil {
			g, _ = s.cgroups.GetDefault(ctx)
		}
		if g != nil {
			concName, concMax = g.Name, g.MaxConcurrency
		}
	}
	return map[string]any{
		"id":                user.ID,
		"email":             user.Email,
		"name":              user.Name,
		"role":              user.Role,
		"status":            user.Status,
		"credits":           user.Credits,
		"recharge_total":    user.RechargeTotal,
		"concurrency_group": concName,
		"concurrency_limit": concMax,
		"checkin_last":      user.CheckinLast,
		"checkin_streak":    user.CheckinStreak,
		"checkin_today":     user.CheckinLast == time.Now().Format("2006-01-02"),
		"invite_code":       user.InviteCode,
		"invite_count":      stats.InviteCount,
		"invite_earned":     stats.InviteEarned,
	}, nil
}

func (s *AuthService) Checkin(ctx context.Context, userID string) (*repo.CheckinResult, error) {
	credits, err := s.loadCreditSettings(ctx)
	if err != nil {
		return nil, err
	}
	if !credits.CheckinEnabled {
		return nil, errors.New("签到功能未开启")
	}
	return s.users.DailyCheckin(ctx, userID, credits.CheckinReward)
}

func (s *AuthService) InviteList(ctx context.Context, userID string) ([]repo.InviteRecord, error) {
	credits, err := s.loadCreditSettings(ctx)
	if err != nil {
		return nil, err
	}
	return s.users.InviteList(ctx, userID, credits.InviteReward)
}

func ParseBearer(header string) string {
	if header == "" {
		return ""
	}
	lower := strings.ToLower(header)
	if !strings.HasPrefix(lower, "bearer ") {
		return ""
	}
	return strings.TrimSpace(header[7:])
}

func HashAPIKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (s *AuthService) loadAuthSettings(ctx context.Context) (*AuthSettings, error) {
	openRaw, err := s.settings.GetValue(ctx, "auth.open")
	if err != nil {
		return nil, err
	}
	emailCodeRaw, err := s.settings.GetValue(ctx, "auth.email_code")
	if err != nil {
		return nil, err
	}
	resetRaw, err := s.settings.GetValue(ctx, "auth.allow_password_reset")
	if err != nil {
		return nil, err
	}
	domainsRaw, err := s.settings.GetValue(ctx, "auth.allowed_email_domains")
	if err != nil {
		return nil, err
	}
	return &AuthSettings{
		Open:               parseBoolSetting(openRaw, true),
		EmailCode:          parseBoolSetting(emailCodeRaw, false),
		AllowPasswordReset: parseBoolSetting(resetRaw, false),
		AllowedDomains:     parseCSVSetting(domainsRaw),
	}, nil
}

func (s *AuthService) loadSMTPSettings(ctx context.Context) SMTPConfig {
	host, _ := s.settings.GetValue(ctx, "smtp.host")
	portRaw, _ := s.settings.GetValue(ctx, "smtp.port")
	username, _ := s.settings.GetValue(ctx, "smtp.username")
	password, _ := s.settings.GetValue(ctx, "smtp.password")
	fromAddr, _ := s.settings.GetValue(ctx, "smtp.from_addr")
	useTLSRaw, _ := s.settings.GetValue(ctx, "smtp.use_tls")

	port, _ := strconv.Atoi(strings.TrimSpace(portRaw))
	if port <= 0 {
		port = 587
	}
	// Fall back to username when from_addr is unset (Python core/email_codes.py:92).
	from := strings.TrimSpace(fromAddr)
	if from == "" {
		from = strings.TrimSpace(username)
	}
	return SMTPConfig{
		Host:     strings.TrimSpace(host),
		Port:     port,
		Username: strings.TrimSpace(username),
		Password: password,
		FromAddr: from,
		// use_tls defaults to true to match Python (core/email_codes.py:93).
		UseTLS: parseBoolSetting(useTLSRaw, true),
	}
}

func parseBoolSetting(v string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func parseCSVSetting(v string) []string {
	if strings.TrimSpace(v) == "" {
		return []string{}
	}
	return ValidateAllowedEmailDomains(strings.Split(v, ","))
}

// InviteReward returns the admin-configured 积分 awarded per completed invite
// (falls back to 3). Exposed so the invite page shows the real number.
func (s *AuthService) InviteReward(ctx context.Context) int {
	cs, err := s.loadCreditSettings(ctx)
	if err != nil {
		return 3
	}
	return cs.InviteReward
}

func (s *AuthService) loadCreditSettings(ctx context.Context) (*CreditSettings, error) {
	checkinEnabledRaw, err := s.settings.GetValue(ctx, "credits.checkin_enabled")
	if err != nil {
		return nil, err
	}
	checkinRewardRaw, err := s.settings.GetValue(ctx, "credits.checkin_reward")
	if err != nil {
		return nil, err
	}
	inviteEnabledRaw, err := s.settings.GetValue(ctx, "credits.invite_enabled")
	if err != nil {
		return nil, err
	}
	inviteRewardRaw, err := s.settings.GetValue(ctx, "credits.invite_reward")
	if err != nil {
		return nil, err
	}
	return &CreditSettings{
		CheckinEnabled: parseBoolSetting(checkinEnabledRaw, true),
		CheckinReward:  parseIntSetting(checkinRewardRaw, 3),
		InviteEnabled:  parseBoolSetting(inviteEnabledRaw, true),
		InviteReward:   parseIntSetting(inviteRewardRaw, 3),
	}, nil
}
