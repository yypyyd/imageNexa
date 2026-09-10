package dola

import (
	"context"
	"errors"
	"strings"
)

// BootstrapBrowserSession creates (or refreshes) this server's portable Dola
// browser identity and verifies that the official video skill can mount. It
// never fills the prompt or submits a generation.
func (c *Client) BootstrapBrowserSession(ctx context.Context, account Account) error {
	account = account.normalized()
	if account.Cookie == "" {
		return ErrAuth
	}
	return c.bootstrapBrowserSession(ctx, account)
}

type bootstrapError struct {
	stage string
	cause error
}

func (err *bootstrapError) Error() string {
	return "Dola browser bootstrap " + err.stage + " failed (" + bootstrapCauseClass(err.cause) + ")"
}
func (err *bootstrapError) Unwrap() error { return errors.Join(ErrTemporaryUpstream, err.cause) }

func bootstrapFailure(stage string, err error) error {
	return &bootstrapError{stage: stage, cause: err}
}

// BootstrapStage returns only a fixed non-sensitive stage label for diagnostics.
func BootstrapStage(err error) string {
	var failure *bootstrapError
	if errors.As(err, &failure) {
		return failure.stage
	}
	return "unknown"
}

// bootstrapCauseClass maps browser startup failures to fixed labels without
// exposing paths, proxy URLs, cookies or command-line arguments.
func bootstrapCauseClass(cause error) string {
	if cause == nil {
		return "unknown"
	}
	text := strings.ToLower(cause.Error())
	switch {
	case errors.Is(cause, context.DeadlineExceeded), strings.Contains(text, "timeout"):
		return "timeout"
	case strings.Contains(text, "profile") || strings.Contains(text, "singleton") || strings.Contains(text, "user data"):
		return "profile"
	case strings.Contains(text, "devtools") || strings.Contains(text, "websocket"):
		return "devtools"
	case strings.Contains(text, "required dola cookie rejected: sessionid_ss"):
		return "cookie-sessionid_ss"
	case strings.Contains(text, "required dola cookie rejected: sessionid"):
		return "cookie-sessionid"
	case strings.Contains(text, "required dola cookie rejected: s_v_web_id"):
		return "cookie-s_v_web_id"
	case strings.Contains(text, "permission"):
		return "permission"
	case strings.Contains(text, "exited") || strings.Contains(text, "exit status"):
		return "process-exit"
	case strings.Contains(text, "context") || strings.Contains(text, "canceled"):
		return "context"
	default:
		return "other"
	}
}

func BootstrapDiagnostic(err error) string {
	var failure *bootstrapError
	if errors.As(err, &failure) {
		return bootstrapCauseClass(failure.cause)
	}
	return "unknown"
}
