package oreate

import (
	"errors"
	"strings"
	"testing"
)

func TestClassifyInPageFailureCaptcha(t *testing.T) {
	t.Parallel()
	err := classifyInPageFailure(`{"status":{"code":7350001,"msg":"请输入验证码"}}`)
	if !errors.Is(err, ErrAccountChallenge) || !errors.Is(err, ErrRiskControl) {
		t.Fatalf("captcha payload classified as %v", err)
	}
}

func TestClassifyInPageFailureTemporary(t *testing.T) {
	t.Parallel()
	err := classifyInPageFailure("banti timeout")
	if !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("timeout classified as %v", err)
	}
	if !strings.Contains(err.Error(), "banti timeout") {
		t.Fatalf("timeout message %v", err)
	}
}

func TestExtractInPageStatusCode(t *testing.T) {
	t.Parallel()
	if got := extractInPageStatusCode(`{"status":{"code":7350001,"msg":"x"}}`); got != 7350001 {
		t.Fatalf("got %d", got)
	}
	if got := extractInPageStatusCode("no json"); got != 0 {
		t.Fatalf("empty got %d", got)
	}
}
