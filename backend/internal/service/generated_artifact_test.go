package service

import (
	"context"
	"errors"
	"testing"

	"backend/internal/model"
)

func TestCompletedGenerationRetriesOnlyItsArtifact(t *testing.T) {
	generations, downloads := 0, 0
	data, _, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "adobe"}, {ID: "spare"}}, "", func(ctx context.Context, route model.ModelRoute) ([]byte, string, error) {
		generations++
		data, err := downloadGeneratedArtifact(ctx, "original-result", func(ctx context.Context, url string) ([]byte, error) {
			downloads++
			if url != "original-result" {
				t.Fatal("download switched results")
			}
			if downloads == 1 {
				return nil, errors.New("transient download failure")
			}
			return []byte("image"), nil
		})
		return data, "original-result", err
	})
	if err != nil || string(data) != "image" || generations != 1 || downloads != 2 {
		t.Fatalf("generations=%d downloads=%d data=%q error=%v", generations, downloads, data, err)
	}
}

func TestArtifactFailureCannotBecomeSubmissionRetryOrPendingBytePlus(t *testing.T) {
	calls := 0
	_, _, err := runMediaRouteFailover(context.Background(), []model.ModelRoute{{ID: "adobe"}, {ID: "spare"}}, "", func(ctx context.Context, route model.ModelRoute) ([]byte, string, error) {
		calls++
		workCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		data, err := downloadGeneratedArtifact(workCtx, "result", func(context.Context, string) ([]byte, error) {
			cancel()
			return nil, errors.New("artifact failed")
		})
		return data, "", err
	})
	if calls != 1 || routeFailoverSafe(err) || noRouteFailover(err) || !errors.Is(publicGenerationError(err), ErrProviderTemporary) {
		t.Fatalf("artifact failure lost its boundary: calls=%d error=%v", calls, err)
	}
}
