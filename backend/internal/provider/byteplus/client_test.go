package byteplus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func withAPIBase(t *testing.T, value string) {
	t.Helper()
	previous := apiBaseURL
	apiBaseURL = strings.TrimRight(value, "/")
	t.Cleanup(func() { apiBaseURL = previous })
}

func writeEnvelope(t *testing.T, w http.ResponseWriter, data any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": data}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestModelsAreClosedFiveModelCatalog(t *testing.T) {
	models := Models()
	if len(models) != 5 {
		t.Fatalf("Models() returned %d records, want 5", len(models))
	}
	want := map[string]string{
		"lumina-seedream-5.0-pro":  "7657401949175693322",
		"lumina-gpt-image-2":       "6824519374061285743",
		"lumina-seedream-5.0-lite": "7604761017696141358",
		"lumina-nano-banana-2":     "8162745039814627354",
		"lumina-nano-banana-pro":   "8162745039814627353",
	}
	for publicID, wantUpstreamID := range want {
		spec, ok := LookupModel(publicID)
		if !ok {
			t.Errorf("LookupModel(%q) was rejected", publicID)
			continue
		}
		if spec.ID != wantUpstreamID {
			t.Errorf("LookupModel(%q).ID = %q, want %q", publicID, spec.ID, wantUpstreamID)
		}
		byNumber, ok := LookupModel(wantUpstreamID)
		if !ok || byNumber.Key != spec.Key {
			t.Errorf("numeric lookup %q = %#v, %v; want key %q", wantUpstreamID, byNumber, ok, spec.Key)
		}
	}
	for _, unsupported := range []string{"seedream-4.5", "seedream-4.0", "lumina-seedream-3.0l", "gemini-2.5-flash-image"} {
		if spec, ok := LookupModel(unsupported); ok {
			t.Errorf("unsupported model %q resolved to %#v", unsupported, spec)
		}
	}
	models[0].ID = "mutated"
	if Models()[0].ID == "mutated" {
		t.Fatal("Models returned mutable provider catalog storage")
	}
}

func TestCookieRecognitionAndCSRFDecoding(t *testing.T) {
	cookie := `Cookie: csrfToken=abc%2F123; sessionid=session-value; lang=en`
	if !IsBytePlusCookie(cookie) {
		t.Fatal("valid Lumina cookie was not recognized")
	}
	if got := CSRFTokenFromCookie(cookie); got != "abc/123" {
		t.Fatalf("CSRFTokenFromCookie() = %q, want abc/123", got)
	}
	wrapped := `{"cookie":"csrfToken=csrf; loginToken=login"}`
	if !IsBytePlusCookie(wrapped) {
		t.Fatal("JSON-wrapped Lumina cookie was not recognized")
	}
	for _, invalid := range []string{"", "csrfToken=only; lang=en", "csrfToken=only; sessionid=; locale=en", "sessionid=missing-csrf"} {
		if IsBytePlusCookie(invalid) {
			t.Errorf("IsBytePlusCookie(%q) = true", invalid)
		}
	}
}

func TestNewHTTPClientUsesOnlyExplicitProxy(t *testing.T) {
	direct, err := NewClient("").newHTTPClient(0)
	if err != nil {
		t.Fatal(err)
	}
	directTransport := direct.Transport.(*http.Transport)
	if directTransport.Proxy != nil {
		t.Fatal("direct BytePlus transport inherited an environment proxy")
	}

	proxied, err := NewClient("http://127.0.0.1:8899").newHTTPClient(0)
	if err != nil {
		t.Fatal(err)
	}
	req := &http.Request{URL: &url.URL{Scheme: "https", Host: "ai.byteplus.com"}}
	got, err := proxied.Transport.(*http.Transport).Proxy(req)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "http://127.0.0.1:8899" {
		t.Fatalf("explicit proxy = %q", got)
	}
	if _, err := NewClient("not-a-url").newHTTPClient(0); !errors.Is(err, ErrInvalidParams) {
		t.Fatalf("invalid proxy error = %v, want ErrInvalidParams", err)
	}
}

func TestFetchProfileSendsCookieAndMirroredCSRF(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/api/user/current" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Cookie"); got != "csrfToken=csrf%2Fvalue; sessionid=session" {
			t.Errorf("Cookie = %q", got)
		}
		if got := r.Header.Get("X-Csrf-Token"); got != "csrf/value" {
			t.Errorf("X-Csrf-Token = %q", got)
		}
		if got := r.Header.Get("Origin"); got != webOrigin {
			t.Errorf("Origin = %q", got)
		}
		writeEnvelope(t, w, map[string]any{"user_id": "42", "user_name": "alice", "role": "member"})
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")

	profile, err := NewClient("").FetchProfile(context.Background(), "csrfToken=csrf%2Fvalue; sessionid=session")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || profile["user_id"] != "42" {
		t.Fatalf("profile = %#v, requests = %d", profile, requests)
	}
}

func TestAPIDataRejectsRedirectWithoutReplayingCredentialsOrBody(t *testing.T) {
	var redirectedRequests int
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedRequests++
		t.Errorf("redirect destination received %s %s with Cookie %q and CSRF %q", r.Method, r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("X-Csrf-Token"))
		http.Error(w, "credentials must never reach this server", http.StatusInternalServerError)
	}))
	defer destination.Close()

	var initialBody string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "csrfToken=csrf; sessionid=session" {
			t.Errorf("initial Cookie = %q", got)
		}
		if got := r.Header.Get("X-Csrf-Token"); got != "csrf" {
			t.Errorf("initial X-Csrf-Token = %q", got)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read initial request body: %v", err)
		}
		initialBody = string(raw)
		http.Redirect(w, r, destination.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	withAPIBase(t, origin.URL+"/api")

	_, err := NewClient("").apiData(context.Background(), http.MethodPost, "/redirect", "csrfToken=csrf; sessionid=session", map[string]any{"prompt": "secret body"})
	if !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("redirect error = %v, want ErrTemporaryUpstream", err)
	}
	if redirectedRequests != 0 {
		t.Fatalf("redirect destination received %d requests, want 0", redirectedRequests)
	}
	if initialBody != `{"prompt":"secret body"}` {
		t.Fatalf("initial body = %q", initialBody)
	}
}

func TestFetchProfileRejectsGuestAndAuthStatus(t *testing.T) {
	t.Run("guest", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "guest"})
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		_, err := NewClient("").FetchProfile(context.Background(), "csrfToken=x; sessionid=y")
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("guest error = %v, want ErrAuth", err)
		}
	})

	t.Run("http auth", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		_, err := NewClient("").FetchProfile(context.Background(), "csrfToken=x; sessionid=y")
		if !errors.Is(err, ErrAuth) {
			t.Fatalf("status error = %v, want ErrAuth", err)
		}
	})
}

func TestAPIDataClassifiesParsedErrorsBeforeHTTPAuthStatus(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		response   map[string]any
		want       error
	}{
		{
			name:       "quota code wins over forbidden",
			statusCode: http.StatusForbidden,
			response:   map[string]any{"code": 100000007, "message": "image credits exhausted"},
			want:       ErrQuotaExhausted,
		},
		{
			name:       "nested quota code wins over unauthorized",
			statusCode: http.StatusUnauthorized,
			response: map[string]any{
				"base_response": map[string]any{"error_code": 100000007, "error_msg": "insufficient points"},
			},
			want: ErrQuotaExhausted,
		},
		{
			name:       "risk code wins over forbidden",
			statusCode: http.StatusForbidden,
			response:   map[string]any{"error_code": 100000008, "error_message": "review disapproved by risk control"},
			want:       ErrRiskControl,
		},
		{
			name:       "quota message wins over forbidden",
			statusCode: http.StatusForbidden,
			response:   map[string]any{"message": "quota exceeded for image credits"},
			want:       ErrQuotaExhausted,
		},
		{
			name:       "risk message wins over unauthorized",
			statusCode: http.StatusUnauthorized,
			response:   map[string]any{"message": "risk control rejected this request"},
			want:       ErrRiskControl,
		},
		{
			name:       "explicit expired session is auth",
			statusCode: http.StatusForbidden,
			response:   map[string]any{"code": 403, "message": "session expired, please log in"},
			want:       ErrAuth,
		},
		{
			name:       "bare unauthorized is auth",
			statusCode: http.StatusUnauthorized,
			response:   map[string]any{"message": "request rejected"},
			want:       ErrAuth,
		},
		{
			name:       "generic forbidden is not auth",
			statusCode: http.StatusForbidden,
			response:   map[string]any{"code": 403, "message": "request forbidden"},
			want:       ErrTemporaryUpstream,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.statusCode)
				if err := json.NewEncoder(w).Encode(test.response); err != nil {
					t.Errorf("encode response: %v", err)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, err := NewClient("").apiData(context.Background(), http.MethodGet, "/failure", "csrfToken=x; sessionid=y", nil)
			if !errors.Is(err, test.want) {
				t.Fatalf("apiData error = %v, want %v", err, test.want)
			}
			for _, other := range []error{ErrAuth, ErrQuotaExhausted, ErrRiskControl, ErrInvalidParams, ErrTemporaryUpstream} {
				if other != test.want && errors.Is(err, other) {
					t.Fatalf("apiData error = %v, unexpectedly matches %v", err, other)
				}
			}
		})
	}
}

func TestFetchCreditsBalanceFallback(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Path {
		case "/api/user/get_user_resources":
			writeEnvelope(t, w, map[string]any{"unrelated": true})
		case "/api/inference/get_user_quota":
			if r.URL.Query().Get("type") != "image" {
				t.Errorf("quota type = %q", r.URL.Query().Get("type"))
			}
			writeEnvelope(t, w, map[string]any{"quota": 20, "remain_count": 7})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")

	got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"remaining": 7, "used": 13, "total": 20, "unknown": false, "error": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("balance = %#v, want %#v", got, want)
	}
	if len(paths) != 2 {
		t.Fatalf("quota requests = %#v, want primary then fallback", paths)
	}
	if !strings.Contains(paths[0], "resource_id=lumi%2Fcomputing_points") {
		t.Fatalf("primary quota request = %q, want computing-points resource", paths[0])
	}
}

func TestFetchCreditsBalancePreservesFractionalComputingPoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("resource_id") != "lumi/computing_points" {
			t.Errorf("resource_id = %q", r.URL.Query().Get("resource_id"))
		}
		writeEnvelope(t, w, map[string]any{"quota_list": []any{
			map[string]any{"resource_id": "another/resource", "total": 1000, "used": 0},
			map[string]any{"resource_id": "lumi/computing_points", "total": 80, "used": 15.5},
		}})
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")

	got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"remaining": 64.5, "used": 15.5, "total": 80, "unknown": false, "error": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("balance = %#v, want %#v", got, want)
	}
}

func TestFetchCreditsBalanceReportsNoActivePlanAsKnownZero(t *testing.T) {
	// Observed 2026-09-02 on 119 pool accounts: get_user_resources answers code 0
	// with an empty combos list and a null quota_list, and get_user_quota answers
	// code 200402 "No Active Combos." with is_country_blocked=true.
	t.Run("empty combos payload", func(t *testing.T) {
		var quotaCalls int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/user/get_user_resources":
				writeEnvelope(t, w, map[string]any{"quota_list": nil, "uris": []any{}, "combos": []any{}})
			case "/api/inference/get_user_quota":
				quotaCalls++
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200402, "message": "No Active Combos.", "data": map[string]any{"is_country_blocked": true}})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
		if err != nil {
			t.Fatal(err)
		}
		if unknown, _ := got["unknown"].(bool); unknown || got["remaining"] != 0 || got["error"] != nil {
			t.Fatalf("no-plan balance = %#v, want known zero", got)
		}
		if quotaCalls != 0 {
			t.Fatalf("empty combos payload should be conclusive without the fallback probe, got %d fallback calls", quotaCalls)
		}
	})

	t.Run("fallback verdict", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/user/get_user_resources":
				// No combos field at all: inconclusive on its own.
				writeEnvelope(t, w, map[string]any{"uris": []any{}})
			case "/api/inference/get_user_quota":
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200402, "message": "No Active Combos."})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
		if err != nil {
			t.Fatal(err)
		}
		if unknown, _ := got["unknown"].(bool); unknown || got["remaining"] != 0 {
			t.Fatalf("no-plan fallback balance = %#v, want known zero", got)
		}
	})

	t.Run("active plan still reads real balance", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/user/get_user_resources" {
				http.NotFound(w, r)
				return
			}
			writeEnvelope(t, w, map[string]any{
				"quota_list": []any{map[string]any{"resource_id": "lumi/computing_points", "used": 1, "total": 68}},
				"combos":     []any{map[string]any{"id": 17, "name": "Free"}},
			})
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
		if err != nil {
			t.Fatal(err)
		}
		if got["remaining"] != 67 || got["total"] != 68 {
			t.Fatalf("free plan balance = %#v", got)
		}
	})
}

func TestAPIDataMarksDefinitiveRejections(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantClass  error
		rejected   bool
	}{
		{name: "business code in 200 envelope", statusCode: http.StatusOK, body: `{"code":1500,"message":"internal error"}`, wantClass: ErrTemporaryUpstream, rejected: true},
		{name: "no active combos", statusCode: http.StatusOK, body: `{"code":200402,"message":"No Active Combos."}`, wantClass: ErrNoActivePlan, rejected: true},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, body: `{"message":"too many requests"}`, wantClass: ErrTemporaryUpstream, rejected: true},
		{name: "gateway error", statusCode: http.StatusBadGateway, body: `bad gateway`, wantClass: ErrTemporaryUpstream, rejected: false},
		{name: "non-json success", statusCode: http.StatusOK, body: `<html>challenge</html>`, wantClass: ErrTemporaryUpstream, rejected: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")
			_, err := NewClient("").apiData(context.Background(), http.MethodPost, "/inference/v2/create_task", "csrfToken=x; sessionid=y", map[string]any{})
			if !errors.Is(err, tc.wantClass) {
				t.Fatalf("error = %v, want %v", err, tc.wantClass)
			}
			if errors.Is(err, ErrUpstreamRejected) != tc.rejected {
				t.Fatalf("error = %v, rejected marker = %v, want %v", err, !tc.rejected, tc.rejected)
			}
		})
	}
	if !errors.Is(ErrNoActivePlan, ErrQuotaExhausted) {
		t.Fatal("ErrNoActivePlan must remain a quota-class error for scheduling and API mapping")
	}
}

func TestFetchCreditsBalanceKeepsTransientFailureUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "maintenance", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	got, err := NewClient("").FetchCreditsBalance(context.Background(), "csrfToken=x; sessionid=y")
	if err != nil {
		t.Fatal(err)
	}
	if unknown, _ := got["unknown"].(bool); !unknown || got["remaining"] != nil {
		t.Fatalf("unknown balance = %#v", got)
	}
}
