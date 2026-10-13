package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEnsureBucket(t *testing.T) {
	for _, tc := range []struct {
		name      string
		head, put int
		code      string
		wantError bool
		wantPuts  int
	}{
		{name: "existing", head: 200},
		{name: "missing", head: 404, put: 200, wantPuts: 1},
		{name: "denied", head: 403, wantError: true},
		{name: "concurrent create", head: 404, put: 409, code: "BucketAlreadyOwnedByYou", wantPuts: 1},
		{name: "other owner", head: 404, put: 409, code: "BucketAlreadyExists", wantError: true, wantPuts: 1},
		{name: "failed create", head: 404, put: 500, wantError: true, wantPuts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			puts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/test-bucket" {
					t.Error("unexpected object access")
				}
				switch r.Method {
				case http.MethodHead:
					if puts > 0 {
						w.WriteHeader(http.StatusOK)
					} else {
						w.WriteHeader(tc.head)
					}
				case http.MethodPut:
					puts++
					w.WriteHeader(tc.put)
					_, _ = w.Write([]byte("<Error><Code>" + tc.code + "</Code></Error>"))
				default:
					t.Error("unexpected storage mutation")
				}
			}))
			defer server.Close()
			err := New(server.URL, "test-bucket", "test", "test").EnsureBucket(context.Background())
			if (err != nil) != tc.wantError || puts != tc.wantPuts {
				t.Fatalf("error=%v, create calls=%d", err, puts)
			}
		})
	}
}
