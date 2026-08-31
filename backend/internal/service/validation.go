package service

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MinUsernameLength = 6
	MaxUsernameLength = 24
	MinPasswordLength = 12
	MaxPasswordLength = 64
	MaxPasswordBytes  = 72
)

var (
	usernamePattern      = regexp.MustCompile(`^[A-Za-z0-9]{6,24}$`)
	loginUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9]{1,24}$`)
)

func ValidateEmail(email string) (string, error) {
	normalized := strings.TrimSpace(strings.ToLower(email))
	if normalized == "" {
		return "", errors.New("邮箱不能为空")
	}
	if len(normalized) > 254 {
		return "", errors.New("邮箱长度不能超过 254 个字符")
	}
	addr, err := mail.ParseAddress(normalized)
	if err != nil || strings.TrimSpace(strings.ToLower(addr.Address)) != normalized {
		return "", errors.New("邮箱格式不正确")
	}
	local, domain, ok := strings.Cut(normalized, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "..") || !strings.Contains(domain, ".") {
		return "", errors.New("邮箱格式不正确")
	}
	return normalized, nil
}

func ValidateUsername(username string) (string, error) {
	normalized := strings.TrimSpace(username)
	if normalized == "" {
		return "", errors.New("用户名不能为空")
	}
	length := utf8.RuneCountInString(normalized)
	if length < MinUsernameLength || length > MaxUsernameLength {
		return "", errors.New("用户名长度需为 6 到 24 个字符")
	}
	if !usernamePattern.MatchString(normalized) {
		return "", errors.New("用户名只能使用字母和数字")
	}
	return normalized, nil
}

func ValidatePassword(password string) error {
	length := utf8.RuneCountInString(password)
	if length < MinPasswordLength || length > MaxPasswordLength || len([]byte(password)) > MaxPasswordBytes {
		// bcrypt only consumes 72 bytes. Reject overlong Unicode passphrases
		// instead of silently hashing a truncated prefix.
		return errors.New("密码长度需为 12 到 64 个字符且不能超过 72 字节")
	}

	var hasLetter bool
	var hasUpper bool
	var hasLower bool
	var hasDigit bool
	var hasSymbol bool
	for _, r := range password {
		if unicode.IsSpace(r) {
			return errors.New("密码不能包含空白字符")
		}
		if !isAllowedPasswordRune(r) {
			return errors.New("密码包含不允许的字符")
		}
		if unicode.IsLetter(r) {
			hasLetter = true
			if unicode.IsUpper(r) {
				hasUpper = true
			}
			if unicode.IsLower(r) {
				hasLower = true
			}
		}
		if unicode.IsDigit(r) {
			hasDigit = true
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			hasSymbol = true
		}
	}
	if !hasLetter || !hasUpper || !hasLower || !hasDigit || !hasSymbol {
		return errors.New("密码必须同时包含大写字母、小写字母、数字和符号")
	}
	return nil
}

func isAllowedPasswordRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	switch r {
	case '(', ')', '~', '!', '@', '#', '$', '%', '^', '&', '*', '-', '_', '+', '=', '|',
		'{', '}', '[', ']', ':', ';', '\'', '<', '>', ',', '.', '?', '/':
		return true
	default:
		return false
	}
}

func ValidateLoginIdentifier(identifier string) (string, error) {
	normalized := strings.TrimSpace(identifier)
	if normalized == "" {
		return "", errors.New("账号不能为空")
	}
	if strings.Contains(normalized, "@") {
		return ValidateEmail(normalized)
	}
	if utf8.RuneCountInString(normalized) > MaxUsernameLength {
		return "", errors.New("用户名长度不能超过 24 个字符")
	}
	if !loginUsernamePattern.MatchString(normalized) {
		return "", errors.New("用户名只能使用字母和数字")
	}
	return normalized, nil
}
