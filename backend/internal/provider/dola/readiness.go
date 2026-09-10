package dola

import (
	"context"
	"errors"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

var ErrVideoNotReady = errors.New("dola: video controls not ready")

func verifyDolaControls(ctx context.Context, model, ratio string, duration int) error {
	probe, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := prepareDolaVideoForm(probe, model, "", ratio, duration); err != nil {
		// Use a separate bounded child to inspect the surviving browser after the
		// control wait timed out. Return fixed, non-sensitive error categories.
		inspect, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		var state string
		_ = chromedp.Run(inspect, chromedp.Evaluate(dolaLoginStateJS, &state))
		switch state {
		case "challenge":
			return bootstrapFailure("verification", ErrChallenge)
		case "login_required":
			return bootstrapFailure("login", ErrAuth)
		default:
			return bootstrapFailure("video_controls", ErrVideoNotReady)
		}
	}
	var state string
	if err := chromedp.Run(ctx, chromedp.Evaluate(dolaLoginStateJS, &state)); err != nil {
		return bootstrapFailure("login_state", err)
	}
	if state == "login_required" {
		return bootstrapFailure("login", ErrAuth)
	}
	if state == "challenge" {
		return bootstrapFailure("verification", ErrChallenge)
	}
	return nil
}

const dolaLoginStateJS = `(() => {
 const visible = e => { const r=e.getBoundingClientRect(); return r.width>0 && r.height>0; };
 if (Array.from(document.querySelectorAll('iframe')).some(e => visible(e) && /captcha|verify/i.test(e.src))) return "challenge";
 if (Array.from(document.querySelectorAll('[role="dialog"],button,[role="button"]')).some(e => visible(e) && /^(log in|login|sign in|登录|登入)$/i.test(e.textContent.trim()))) return "login_required";
 return "unavailable";
})()`

// Recovery is only allowed before filling/sending. Both imports and generation
// use this path, so a cold or evicted browser gets the same readiness checks.
func prepareDolaControlsWithRecovery(ctx context.Context, model, ratio string, duration int) error {
	err := verifyDolaControls(ctx, model, ratio, duration)
	if err == nil || !errors.Is(err, ErrVideoNotReady) {
		return err
	}
	if err = chromedp.Run(ctx, chromedp.ActionFunc(func(action context.Context) error {
		_, err := page.AddScriptToEvaluateOnNewDocument(dolaDuration30Script).Do(action)
		return err
	}), chromedp.Reload()); err != nil {
		return bootstrapFailure("readiness_reload", err)
	}
	if err = waitDolaDocument(ctx); err != nil {
		return err
	}
	return verifyDolaControls(ctx, model, ratio, duration)
}
