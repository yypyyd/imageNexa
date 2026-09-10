package service

import (
	"context"
	"errors"
	"time"

	"backend/internal/model"
	"backend/internal/provider/dola"
)

func dolaReadinessVerdict(err error) (string, string) {
	switch {
	case err == nil:
		return "ready", ""
	case errors.Is(err, dola.ErrAuth):
		return "login_required", "登录未恢复，请更新完整 Cookie 后重新验证"
	case errors.Is(err, dola.ErrChallenge):
		return "challenge", "需要在 Dola 完成人机验证后重新验证"
	case errors.Is(err, dola.ErrVideoNotReady):
		return "retry", "协议会话未就绪，将自动重试；也可更新完整 Cookie"
	default:
		return "retry", "协议接口或网络暂不可用，将自动重试"
	}
}

func (s *TokenService) verifyDolaReadiness(id string, force bool) {
	// Only the claimed worker may publish readiness. Both the credential and
	// probe IDs must still match; disabling/reimporting invalidates its result.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if s.tokens == nil {
		return
	}
	if s.sem != nil {
		select {
		case s.sem <- struct{}{}:
			defer func() { <-s.sem }()
		case <-ctx.Done():
			return
		}
	}
	a, err := s.tokens.ClaimDolaReadiness(ctx, id, force)
	if err != nil || a == nil {
		return
	}
	verdict, detail := "retry", "验证进程异常，将自动重试"
	defer func() {
		_ = recover()
		finish, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, finishErr := s.tokens.FinishDolaReadiness(finish, *a, verdict, detail)
		recordBookkeepingError("finish Dola readiness", finishErr)
	}()
	if s.dola == nil {
		detail = "Dola 协议客户端未配置"
		return
	}
	s.applyProxy(ctx)
	err = s.dola.VerifyProtocolSession(ctx, dolaAccountFromToken(*a))
	verdict, detail = dolaReadinessVerdict(err)
}

func (s *TokenService) ReprobeDolaReadiness(ctx context.Context) {
	if s.tokens == nil || s.dola == nil || !s.dolaVerifying.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.dolaVerifying.Store(false)
		items, err := s.tokens.DolaReadinessCandidates(ctx)
		if err != nil {
			return
		}
		for _, a := range items {
			if ctx.Err() != nil {
				return
			}
			if !model.DolaAccountReady(a) {
				s.verifyDolaReadiness(a.ID, false)
			}
		}
	}()
}

func dolaReadinessView(a model.TokenAccount) (string, string) {
	if a.Pool != "dola" {
		return "", ""
	}
	if model.DolaAccountReady(a) {
		return "ready", "协议登录与签名验证通过"
	}
	state, _ := a.Meta["dola_readiness"].(string)
	if state == "ready" || state == "" {
		state = "pending"
	}
	detail, _ := a.Meta["dola_readiness_detail"].(string)
	return state, detail
}
