package service

import (
	"context"
	"errors"
	"time"
)

var errGeneratedArtifactUnavailable = errors.New("completed generation artifact unavailable")

// A successful generation is a no-resubmit boundary even when its artifact
// cannot be retrieved. Retrying a download is safe; selecting another account
// or provider here could create and bill a second generation.
func downloadGeneratedArtifact(ctx context.Context, url string, download func(context.Context, string) ([]byte, error)) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			break
		}
		data, err := download(ctx, url)
		if err == nil && len(data) > 0 {
			return data, nil
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}
	// Do not retain signed URLs or upstream error bodies in public errors.
	return nil, errors.Join(errGeneratedArtifactUnavailable, ErrProviderTemporary)
}
