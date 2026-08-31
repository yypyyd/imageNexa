package byteplus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func decodeJSONMap(t *testing.T, reader io.Reader) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Errorf("decode request: %v", err)
	}
	return value
}

func inputValues(t *testing.T, payload map[string]any) map[string]string {
	t.Helper()
	got := make(map[string]string)
	inputs, ok := payload["inputs"].([]any)
	if !ok {
		t.Fatalf("payload inputs = %#v", payload["inputs"])
	}
	for _, raw := range inputs {
		input, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("payload input = %#v", raw)
		}
		got[stringValue(input["internal_name"])] = stringValue(input["value"])
	}
	return got
}

func TestGenerateImageFiveModelPayloads(t *testing.T) {
	tests := []struct {
		name       string
		publicID   string
		resolution string
		ratio      string
		quality    string
		wantInputs map[string]string
	}{
		{
			name: "Seedream 5 Pro", publicID: "lumina-seedream-5.0-pro", resolution: "2K", ratio: "16:9",
			wantInputs: map[string]string{
				"prompt": "draw a lighthouse", "inner_min_ratio": "0.07", "inner_max_ratio": "16",
				"optimize_prompt_options.thinking": "enabled", "optimize_prompt": "true",
				"size": "2048x1152", "seed": "-1",
			},
		},
		{
			name: "GPT Image 2", publicID: "lumina-gpt-image-2", resolution: "4K", ratio: "9:16", quality: "high",
			wantInputs: map[string]string{"prompt": "draw a lighthouse", "quality": "high", "image_size": "2160x3840"},
		},
		{
			name: "Seedream 5 Lite", publicID: "lumina-seedream-5.0-lite", resolution: "2K", ratio: "4:3",
			wantInputs: map[string]string{
				"prompt": "draw a lighthouse", "force_single": "false", "seed": "-1", "min_ratio": "0.07",
				"max_ratio": "16", "cot_mode": "enable", "close_search": "false", "use_pre_llm": "true",
				"allow_llm_fallback": "true", "width": "2048", "height": "1536", "model_version": "general_v5.0_M",
				"pre_vlm_version": "seed_x2i_50m_pe_mix_modelapi",
			},
		},
		{
			name: "Nano Banana 2", publicID: "lumina-nano-banana-2", resolution: "2K", ratio: "3:4",
			wantInputs: map[string]string{"prompt": "draw a lighthouse", "aspect_ratio": "3:4", "image_size": "2K"},
		},
		{
			name: "Nano Banana Pro", publicID: "lumina-nano-banana-pro", resolution: "1K", ratio: "1:1",
			wantInputs: map[string]string{"prompt": "draw a lighthouse", "aspect_ratio": "1:1", "image_size": "1K"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var createPayload map[string]any
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "user-1", "role": "member"})
				case "/api/inference/v2/create_task":
					if r.Method != http.MethodPost {
						t.Errorf("create method = %s", r.Method)
					}
					if got := r.Header.Get("Cookie"); got != "csrfToken=csrf; sessionid=session" {
						t.Errorf("create Cookie = %q", got)
					}
					if got := r.Header.Get("X-Csrf-Token"); got != "csrf" {
						t.Errorf("create X-Csrf-Token = %q", got)
					}
					decoded := decodeJSONMap(t, r.Body)
					mu.Lock()
					createPayload = decoded
					mu.Unlock()
					writeEnvelope(t, w, map[string]any{"parent_task_id": "task-1"})
				case "/api/inference/task/query_task_list":
					query := decodeJSONMap(t, r.Body)
					if got := fmt.Sprint(query["page_num"]); got != "1" {
						t.Errorf("poll page_num = %s", got)
					}
					writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
						"status": "complete",
						"children": []any{map[string]any{"status": "complete", "multi_outputs": []any{
							map[string]any{"value": server.URL + "/api/resource-utils/task-1.png", "format": "image/png"},
						}}},
					}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, meta, err := NewClient("").GenerateImage(context.Background(), "csrfToken=csrf; sessionid=session", ImageRequest{
				Model: tt.publicID, Prompt: "draw a lighthouse", Size: "1024x1024", Resolution: tt.resolution,
				AspectRatio: tt.ratio, Quality: tt.quality,
			})
			if err != nil {
				t.Fatal(err)
			}
			if meta["image_url"] != server.URL+"/api/resource-utils/task-1.png" || meta["status"] != "complete" {
				t.Fatalf("generation meta = %#v", meta)
			}

			mu.Lock()
			payload := createPayload
			mu.Unlock()
			if payload == nil {
				t.Fatal("create_task was not called")
			}
			if fmt.Sprint(payload["count"]) != "1" {
				t.Errorf("count = %#v", payload["count"])
			}
			if _, exists := payload["sub_task_count"]; exists {
				t.Error("model metadata sub_task_count must not be sent in create_task")
			}
			if payload["inference_type"] != "t2i" || fmt.Sprint(payload["request_source"]) != "1" {
				t.Errorf("task envelope = %#v", payload)
			}
			spec, _ := LookupModel(tt.publicID)
			config, _ := payload["inference_config"].(map[string]any)
			if config["inference_pipeline"] != "vproxy_overpass" || config["inference_id"] != spec.ID ||
				config["inference_ver_id"] != spec.ID || config["req_key"] != spec.ReqKey || config["name"] != spec.Name {
				t.Errorf("inference_config = %#v, spec = %#v", config, spec)
			}
			if got := inputValues(t, payload); !reflect.DeepEqual(got, tt.wantInputs) {
				t.Errorf("input values = %#v, want %#v", got, tt.wantInputs)
			}
		})
	}
}

func TestGenerateImageFiveModelsPreserveNonPNGMagic(t *testing.T) {
	jpeg := append([]byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10}, []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")...)
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 32)...)
	avif := append([]byte{0, 0, 0, 24}, []byte("ftypavif")...)
	avif = append(avif, make([]byte, 32)...)
	tests := []struct {
		model       string
		fixture     []byte
		contentType string
	}{
		{model: "lumina-seedream-5.0-pro", fixture: jpeg, contentType: "image/jpeg"},
		{model: "lumina-gpt-image-2", fixture: webp, contentType: "image/webp"},
		{model: "lumina-seedream-5.0-lite", fixture: avif, contentType: "image/avif"},
		{model: "lumina-nano-banana-2", fixture: jpeg, contentType: "image/jpeg"},
		{model: "lumina-nano-banana-pro", fixture: webp, contentType: "image/webp"},
	}
	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "user-1", "role": "member"})
				case "/api/inference/v2/create_task":
					writeEnvelope(t, w, map[string]any{"parent_task_id": "task-format"})
				case "/api/inference/task/query_task_list":
					writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
						"status": "complete",
						"children": []any{map[string]any{"status": "complete", "multi_outputs": []any{
							map[string]any{"value": server.URL + "/api/resource-utils/output.png", "format": "image/png"},
						}}},
					}}})
				case "/api/resource-utils/output.png":
					// Deliberately stale: payload magic, not this header or suffix,
					// must drive the returned and persisted media type.
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(tt.fixture)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			asset, meta, err := NewClient("").GenerateImage(context.Background(), "csrfToken=csrf; sessionid=session", ImageRequest{
				Model: tt.model, Prompt: "draw a lighthouse", Resolution: "1K", AspectRatio: "1:1", DownloadResult: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(asset, tt.fixture) {
				t.Fatalf("asset bytes changed: got %d bytes, want %d", len(asset), len(tt.fixture))
			}
			if got := stringValue(meta["content_type"]); got != tt.contentType {
				t.Fatalf("content_type = %q, want magic-detected %q; meta=%#v", got, tt.contentType, meta)
			}
		})
	}
}

func TestResumeImageTaskNeverCreatesAnotherTask(t *testing.T) {
	var createCalls atomic.Int32
	var queryCalls atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/inference/v2/create_task":
			createCalls.Add(1)
			http.Error(w, "must not submit", http.StatusInternalServerError)
		case "/api/inference/task/query_task_list":
			queryCalls.Add(1)
			query := decodeJSONMap(t, r.Body)
			ids, _ := query["ids"].([]any)
			if len(ids) != 1 || fmt.Sprint(ids[0]) != "accepted-parent-1" {
				t.Fatalf("resume ids = %#v", query["ids"])
			}
			writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
				"status": "complete",
				"children": []any{map[string]any{"status": "complete", "multi_outputs": []any{
					map[string]any{"value": server.URL + "/api/resource-utils/result.png", "format": "image/png"},
				}}},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")

	_, meta, err := NewClient("").ResumeImageTask(context.Background(), "csrfToken=csrf; sessionid=session", "accepted-parent-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if meta["task_id"] != "accepted-parent-1" || meta["status"] != "complete" {
		t.Fatalf("resume metadata = %#v", meta)
	}
	if got := createCalls.Load(); got != 0 {
		t.Fatalf("create_task calls = %d, want 0", got)
	}
	if got := queryCalls.Load(); got != 1 {
		t.Fatalf("query_task_list calls = %d, want 1", got)
	}
}

func TestRequiredCreditsMatchesLuminaBillingRules(t *testing.T) {
	tests := []struct {
		name    string
		request ImageRequest
		want    float64
	}{
		{name: "Seedream Pro 1K", request: ImageRequest{Model: "lumina-seedream-5.0-pro", Resolution: "1K", AspectRatio: "1:1"}, want: 9},
		{name: "Seedream Pro 2K widescreen stays below pixel breakpoint", request: ImageRequest{Model: "lumina-seedream-5.0-pro", Resolution: "2K", AspectRatio: "16:9"}, want: 9},
		{name: "Seedream Pro 2K square", request: ImageRequest{Model: "lumina-seedream-5.0-pro", Resolution: "2K", AspectRatio: "1:1"}, want: 18},
		{name: "Seedream Pro extra references", request: ImageRequest{Model: "lumina-seedream-5.0-pro", Resolution: "1K", AspectRatio: "1:1", References: make([][]byte, 4)}, want: 9.9},
		{name: "GPT low 1K", request: ImageRequest{Model: "lumina-gpt-image-2", Resolution: "1K", AspectRatio: "1:1", Quality: "low"}, want: 1},
		{name: "GPT medium small compatibility canvas", request: ImageRequest{Model: "lumina-gpt-image-2", Resolution: "2K", AspectRatio: "4:3", Quality: "medium"}, want: 8},
		{name: "GPT medium 2K square", request: ImageRequest{Model: "lumina-gpt-image-2", Resolution: "2K", AspectRatio: "1:1", Quality: "medium"}, want: 25},
		{name: "GPT high 4K landscape", request: ImageRequest{Model: "lumina-gpt-image-2", Resolution: "4K", AspectRatio: "16:9", Quality: "high"}, want: 230},
		{name: "GPT high 4K square compatibility canvas", request: ImageRequest{Model: "lumina-gpt-image-2", Resolution: "4K", AspectRatio: "1:1", Quality: "high"}, want: 96},
		{name: "Seedream Lite", request: ImageRequest{Model: "lumina-seedream-5.0-lite", Resolution: "2K", AspectRatio: "1:1"}, want: 3.5},
		{name: "Nano 2 2K", request: ImageRequest{Model: "lumina-nano-banana-2", Resolution: "2K", AspectRatio: "1:1"}, want: 12},
		{name: "Nano Pro 4K", request: ImageRequest{Model: "lumina-nano-banana-pro", Resolution: "4K", AspectRatio: "1:1"}, want: 24},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RequiredCredits(tc.request)
			if err != nil {
				t.Fatalf("RequiredCredits() error = %v", err)
			}
			if got != tc.want {
				t.Fatalf("RequiredCredits() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResolutionOverridesOpenAISize(t *testing.T) {
	spec, _ := LookupModel("lumina-nano-banana-pro")
	payload, err := buildCreateTaskPayload(spec, ImageRequest{
		Prompt: "test", Size: "1024x1024", Resolution: "4K", AspectRatio: "16:9",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if got := inputValues(t, decoded)["image_size"]; got != "4K" {
		t.Fatalf("image_size = %q, want resolution override 4K", got)
	}
}

func TestGPTImageSizeCompatibilityMatrix(t *testing.T) {
	tests := []struct {
		name  string
		size  string
		ratio string
		want  string
	}{
		{name: "upstream default", want: "1024x1024"},
		{name: "exact upstream size is preserved", size: "2160x3840", ratio: "1:1", want: "2160x3840"},
		{name: "1K square", size: "1K", ratio: "1:1", want: "1024x1024"},
		{name: "1K landscape stays below 2K", size: "1K", ratio: "16:9", want: "1536x1024"},
		{name: "1K portrait is not promoted to 4K", size: "1K", ratio: "9:16", want: "1024x1536"},
		{name: "2K square", size: "2K", ratio: "1:1", want: "2048x2048"},
		{name: "2K landscape", size: "2K", ratio: "16:9", want: "2048x1152"},
		{name: "2K portrait uses closest smaller canvas", size: "2K", ratio: "9:16", want: "1024x1536"},
		{name: "4K landscape", size: "4K", ratio: "4:3", want: "3840x2160"},
		{name: "4K portrait", size: "4K", ratio: "3:4", want: "2160x3840"},
		{name: "4K square uses largest square option", size: "4K", ratio: "1:1", want: "2048x2048"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := gptImageSize(tt.size, tt.ratio)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("gptImageSize(%q, %q) = %q, want %q", tt.size, tt.ratio, got, tt.want)
			}
		})
	}
}

func TestGPTImageQualityDoesNotPromoteCanvas(t *testing.T) {
	spec, _ := LookupModel("lumina-gpt-image-2")
	payload, err := buildCreateTaskPayload(spec, ImageRequest{
		Prompt: "test", Resolution: "1K", AspectRatio: "9:16", Quality: "high",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(payload)
	var decoded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	inputs := inputValues(t, decoded)
	if inputs["quality"] != "high" {
		t.Fatalf("quality = %q, want high", inputs["quality"])
	}
	if inputs["image_size"] != "1024x1536" {
		t.Fatalf("image_size = %q, want 1024x1536", inputs["image_size"])
	}
}

func TestReferenceLimitsAreEnforcedBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		model string
		count int
	}{
		{"lumina-seedream-5.0-pro", 11},
		{"lumina-seedream-5.0-lite", 11},
		{"lumina-gpt-image-2", 15},
		{"lumina-nano-banana-2", 15},
		{"lumina-nano-banana-pro", 15},
	} {
		refs := make([][]byte, tc.count)
		for index := range refs {
			refs[index] = []byte("image")
		}
		_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
			Model: tc.model, Prompt: "test", References: refs,
		})
		if !errors.Is(err, ErrInvalidParams) {
			t.Errorf("%s with %d refs error = %v, want ErrInvalidParams", tc.model, tc.count, err)
		}
	}
}

func TestImageXUploadCreateAndPollChain(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nreference")
	storeURI := "folder/ref+one.png"
	var sawApply, sawUpload, sawCommit, sawRisk, sawReference bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/user/current":
			writeEnvelope(t, w, map[string]any{"user_id": "user-id", "user_name": "user name/!", "role": "member"})
		case r.URL.Path == "/api/egress_gateway/imagex_upload_token":
			writeEnvelope(t, w, map[string]any{
				"AccessKeyId": "AKID", "SecretAccessKey": "SECRET", "SessionToken": "SESSION",
			})
		case r.URL.Path == "/" && r.URL.Query().Get("Action") == "ApplyImageUpload":
			sawApply = true
			if r.URL.Query().Get("ServiceId") != imageXServiceID || r.URL.Query().Get("FileSize") != fmt.Sprint(len(png)) {
				t.Errorf("apply query = %s", r.URL.RawQuery)
			}
			if got := r.URL.Query().Get("StoreKeys"); !strings.HasPrefix(got, "user-id-") {
				t.Errorf("StoreKeys = %q, want user_id prefix", got)
			}
			auth := r.Header.Get("Authorization")
			if !strings.Contains(auth, "AWS4-HMAC-SHA256") || !strings.Contains(auth, "SignedHeaders=host;x-amz-date;x-amz-security-token") {
				t.Errorf("apply Authorization = %q", auth)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ResponseMetadata": map[string]any{},
				"Result": map[string]any{"UploadAddress": map[string]any{
					"StoreInfos":  []any{map[string]any{"StoreUri": storeURI, "Auth": "store-auth"}},
					"UploadHosts": []string{server.URL}, "SessionKey": "upload-session",
					"UploadHeader": map[string]string{"X-Test-Upload": "present"},
				}},
			})
		case r.URL.EscapedPath() == "/upload/v1/folder/ref+one.png":
			sawUpload = true
			raw, _ := io.ReadAll(r.Body)
			if !bytes.Equal(raw, png) {
				t.Errorf("upload body = %q", raw)
			}
			if r.Header.Get("Authorization") != "store-auth" || r.Header.Get("X-Test-Upload") != "present" {
				t.Errorf("upload headers = %#v", r.Header)
			}
			if got := r.Header.Get("Content-CRC32"); got != fmt.Sprintf("%08x", crc32.ChecksumIEEE(png)) {
				t.Errorf("Content-CRC32 = %q", got)
			}
			if got := r.Header.Get("X-Storage-U"); got != "ByteArtist_User_user%20name%2F!" {
				t.Errorf("X-Storage-U = %q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 2000})
		case r.URL.Path == "/" && r.URL.Query().Get("Action") == "CommitImageUpload":
			sawCommit = true
			if !strings.Contains(r.Header.Get("Authorization"), "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token") {
				t.Errorf("commit Authorization = %q", r.Header.Get("Authorization"))
			}
			body := decodeJSONMap(t, r.Body)
			if body["SessionKey"] != "upload-session" {
				t.Errorf("commit body = %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ResponseMetadata": map[string]any{}})
		case r.URL.Path == "/api/egress_gateway/risk_predict":
			sawRisk = true
			body := decodeJSONMap(t, r.Body)
			uris, _ := body["imagex_uris"].([]any)
			if fmt.Sprint(body["risk_check_type"]) != "1" || len(uris) != 1 || uris[0] != storeURI {
				t.Errorf("risk_predict body = %#v", body)
			}
			writeEnvelope(t, w, map[string]any{"hit": false})
		case r.URL.Path == "/api/inference/v2/create_task":
			payload := decodeJSONMap(t, r.Body)
			if payload["inference_type"] != "i2i" {
				t.Errorf("inference_type = %#v", payload["inference_type"])
			}
			inputs, _ := payload["inputs"].([]any)
			for _, raw := range inputs {
				input, _ := raw.(map[string]any)
				if input["format"] == "image_upload" {
					sawReference = input["internal_name"] == "image" && input["type"] == "uri" && input["value"] == storeURI
				}
			}
			writeEnvelope(t, w, map[string]any{"parent_task_id": "task-with-ref"})
		case r.URL.Path == "/api/inference/task/query_task_list":
			writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
				"status": "complete", "multi_outputs": `[{"value":"/resource-utils/ref-result.png","format":"image/png"}]`,
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	previousImageX := imageXEndpointURL
	imageXEndpointURL = server.URL
	t.Cleanup(func() { imageXEndpointURL = previousImageX })

	_, meta, err := NewClient("").GenerateImage(context.Background(), "csrfToken=csrf; sessionid=session", ImageRequest{
		Model: "lumina-seedream-5.0-pro", Prompt: "edit", Resolution: "1K", References: [][]byte{png},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawApply || !sawUpload || !sawCommit || !sawRisk || !sawReference {
		t.Fatalf("chain apply=%v upload=%v commit=%v risk=%v reference=%v", sawApply, sawUpload, sawCommit, sawRisk, sawReference)
	}
	if meta["image_url"] != server.URL+"/api/resource-utils/ref-result.png" {
		t.Fatalf("image_url = %#v", meta["image_url"])
	}
}

func TestImageXSigV4IncludesHostAndIsDeterministic(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://imagex-ap-southeast-1.bytevcloudapi.com/?Action=ApplyImageUpload&Version=2018-08-01", nil)
	if err != nil {
		t.Fatal(err)
	}
	token := imageXToken{AccessKeyID: "AKID", SecretAccessKey: "SECRET", SessionToken: "SESSION"}
	signImageXRequest(req, token, nil, time.Date(2026, time.August, 31, 1, 2, 3, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKID/20260831/ap-southeast-1/imagex/aws4_request, SignedHeaders=host;x-amz-date;x-amz-security-token, Signature=87346d622da061d712b026a48ee81b688581635f7c5eb066165b07ab4d153859"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

func TestImageXUploadHostAllowlist(t *testing.T) {
	previous := imageXEndpointURL
	imageXEndpointURL = "http://127.0.0.1:43210"
	t.Cleanup(func() { imageXEndpointURL = previous })

	allowed := []string{
		"http://127.0.0.1:43210",
		"https://upload.bytepluses.com",
		"https://a.b.bytepluscdn.com",
		"https://imagex-ap-southeast-1.bytevcloudapi.com",
	}
	for _, host := range allowed {
		if _, err := imageXUploadURL(host, "folder/file.png"); err != nil {
			t.Errorf("trusted upload host %q rejected: %v", host, err)
		}
	}
	for _, host := range []string{
		"http://upload.bytepluses.com", "https://bytepluses.com.evil.example", "https://evil.example", "https://user@upload.bytepluses.com",
	} {
		if _, err := imageXUploadURL(host, "folder/file.png"); !errors.Is(err, ErrTemporaryUpstream) {
			t.Errorf("untrusted upload host %q error = %v", host, err)
		}
	}
}

func TestImageXEphemeralAuthFailureDoesNotInvalidateLuminaCookie(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"signature expired"}`, http.StatusForbidden)
	}))
	defer server.Close()
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewClient("").doImageX(context.Background(), req, 8<<10)
	if !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("ImageX 403 error = %v, want ErrTemporaryUpstream", err)
	}
	if errors.Is(err, ErrAuth) {
		t.Fatalf("ImageX 403 must not invalidate the durable Lumina Cookie: %v", err)
	}
}

func TestRiskResultRequiresBooleanHit(t *testing.T) {
	if err := validateRiskResult(map[string]any{"hit": false}); err != nil {
		t.Fatalf("hit=false error = %v", err)
	}
	if err := validateRiskResult(map[string]any{"hit": true}); !errors.Is(err, ErrRiskControl) {
		t.Fatalf("hit=true error = %v, want ErrRiskControl", err)
	}
	for _, value := range []any{nil, map[string]any{}, map[string]any{"hit": nil}, map[string]any{"hit": "false"}} {
		if err := validateRiskResult(value); !errors.Is(err, ErrTemporaryUpstream) {
			t.Errorf("validateRiskResult(%#v) error = %v, want ErrTemporaryUpstream", value, err)
		}
	}
}

func TestReferenceUploadRequiresProfileUserName(t *testing.T) {
	var unexpectedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/user/current" {
			writeEnvelope(t, w, map[string]any{"user_id": "user-id", "role": "member"})
			return
		}
		unexpectedPath = r.URL.Path
		http.NotFound(w, r)
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
		Model: "lumina-seedream-5.0-pro", Prompt: "edit", References: [][]byte{{1}},
	})
	if !errors.Is(err, ErrTemporaryUpstream) || !strings.Contains(err.Error(), "user name") {
		t.Fatalf("missing user_name error = %v", err)
	}
	if unexpectedPath != "" {
		t.Fatalf("missing user_name continued to %q", unexpectedPath)
	}
}

func TestPollHandlesMultiOutputAndTerminalErrors(t *testing.T) {
	task := map[string]any{
		"resources": map[string]any{"resource-id": map[string]any{"url": "https://cdn.bytepluscdn.com/result.png"}},
		"children": []any{map[string]any{"multi_outputs": []any{
			map[string]any{"value": "resource-id", "format": "image/png"},
		}}},
	}
	gotURL, gotType := taskImageOutput(task)
	if gotURL != "https://cdn.bytepluscdn.com/result.png" || gotType != "image/png" {
		t.Fatalf("taskImageOutput() = %q, %q", gotURL, gotType)
	}

	for _, tc := range []struct {
		status string
		want   error
	}{
		{"limit", ErrQuotaExhausted}, {"risk", ErrRiskControl}, {"invalid_param", ErrInvalidParams},
		{"no_face_detected", ErrInvalidParams}, {"failed", ErrTemporaryUpstream},
	} {
		if err := taskStatusError(tc.status, "failure"); !errors.Is(err, tc.want) {
			t.Errorf("status %q error = %v, want %v", tc.status, err, tc.want)
		}
	}
}

func TestCreateAndPollTimeouts(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		previous := createTaskTimeout
		createTaskTimeout = 25 * time.Millisecond
		t.Cleanup(func() { createTaskTimeout = previous })
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/user/current" {
				writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				return
			}
			time.Sleep(100 * time.Millisecond)
			writeEnvelope(t, w, map[string]any{"parent_task_id": "too-late"})
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
			Model: "lumina-nano-banana-2", Prompt: "test",
		})
		if !errors.Is(err, ErrTaskSubmissionUnknown) || !errors.Is(err, ErrTemporaryUpstream) {
			t.Fatalf("create timeout error = %v, want ErrTaskSubmissionUnknown and ErrTemporaryUpstream", err)
		}
	})

	t.Run("poll", func(t *testing.T) {
		previousWait, previousPoll := maxGenerationWait, pollInterval
		maxGenerationWait, pollInterval = 35*time.Millisecond, 5*time.Millisecond
		t.Cleanup(func() { maxGenerationWait, pollInterval = previousWait, previousPoll })
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/api/user/current":
				writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
			case "/api/inference/v2/create_task":
				writeEnvelope(t, w, map[string]any{"parent_task_id": "task"})
			case "/api/inference/task/query_task_list":
				writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{"status": "running"}}})
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		withAPIBase(t, server.URL+"/api")
		_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
			Model: "lumina-nano-banana-pro", Prompt: "test",
		})
		if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("poll timeout error = %v", err)
		}
	})
}

func TestAmbiguousCreateTaskOutcomeCannotBeResubmitted(t *testing.T) {
	for _, failure := range []string{"http 503", "network disconnect", "missing task id"} {
		t.Run(failure, func(t *testing.T) {
			var createCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					createCalls.Add(1)
					switch failure {
					case "http 503":
						http.Error(w, "busy", http.StatusServiceUnavailable)
					case "network disconnect":
						hijacker, ok := w.(http.Hijacker)
						if !ok {
							t.Error("test server does not support connection hijacking")
							return
						}
						conn, _, err := hijacker.Hijack()
						if err != nil {
							t.Errorf("hijack create connection: %v", err)
							return
						}
						_ = conn.Close()
					case "missing task id":
						writeEnvelope(t, w, map[string]any{"accepted": true})
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-2", Prompt: "test",
			})
			if !errors.Is(err, ErrTaskSubmissionUnknown) || !errors.Is(err, ErrTemporaryUpstream) {
				t.Fatalf("ambiguous create error = %v, want ErrTaskSubmissionUnknown and ErrTemporaryUpstream", err)
			}
			if errors.Is(err, ErrTaskAccepted) {
				t.Fatalf("ambiguous create error incorrectly claims a known task id: %v", err)
			}
			if createCalls.Load() != 1 {
				t.Fatalf("create_task calls = %d, want 1", createCalls.Load())
			}
		})
	}
}

func TestExplicitCreateTaskErrorsRetainTheirClass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		httpStatus int
		code       int
		message    string
		want       error
	}{
		{name: "auth", httpStatus: http.StatusUnauthorized, code: http.StatusUnauthorized, message: "session expired", want: ErrAuth},
		{name: "quota", httpStatus: http.StatusForbidden, code: 100000007, message: "credits exhausted", want: ErrQuotaExhausted},
		{name: "risk", httpStatus: http.StatusForbidden, code: 100000008, message: "risk rejected", want: ErrRiskControl},
		{name: "invalid", httpStatus: http.StatusBadRequest, code: 1000000023, message: "invalid parameter", want: ErrInvalidParams},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.httpStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"code": tc.code, "message": tc.message})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-2", Prompt: "test",
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("create %s error = %v, want %v", tc.name, err, tc.want)
			}
			if errors.Is(err, ErrTaskSubmissionUnknown) {
				t.Fatalf("explicit create %s error was marked ambiguous: %v", tc.name, err)
			}
		})
	}
}

func TestGenerateRetriesAcceptedTaskPollWithoutResubmitting(t *testing.T) {
	previousWait, previousPoll := maxGenerationWait, pollInterval
	maxGenerationWait, pollInterval = 500*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { maxGenerationWait, pollInterval = previousWait, previousPoll })

	for _, failure := range []string{"http 503", "network disconnect"} {
		t.Run(failure, func(t *testing.T) {
			var createCalls atomic.Int32
			var pollCalls atomic.Int32
			var assetCalls atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					createCalls.Add(1)
					writeEnvelope(t, w, map[string]any{"parent_task_id": "accepted-task"})
				case "/api/inference/task/query_task_list":
					if pollCalls.Add(1) == 1 {
						if failure == "network disconnect" {
							hijacker, ok := w.(http.Hijacker)
							if !ok {
								t.Error("test server does not support connection hijacking")
								return
							}
							conn, _, err := hijacker.Hijack()
							if err != nil {
								t.Errorf("hijack poll connection: %v", err)
								return
							}
							_ = conn.Close()
							return
						}
						http.Error(w, "busy", http.StatusServiceUnavailable)
						return
					}
					writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
						"status": "complete", "output": server.URL + "/api/resource-utils/accepted-task.png",
					}}})
				case "/api/resource-utils/accepted-task.png":
					assetCalls.Add(1)
					http.Error(w, "URL-only generation must not download", http.StatusInternalServerError)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, meta, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-2", Prompt: "test",
			})
			if err != nil {
				t.Fatal(err)
			}
			if createCalls.Load() != 1 {
				t.Fatalf("create_task calls = %d, want 1", createCalls.Load())
			}
			if pollCalls.Load() != 2 {
				t.Fatalf("query_task_list calls = %d, want 2", pollCalls.Load())
			}
			if assetCalls.Load() != 0 {
				t.Fatalf("URL-only generation downloaded asset %d times, want 0", assetCalls.Load())
			}
			if meta["task_id"] != "accepted-task" {
				t.Fatalf("generation metadata = %#v", meta)
			}
		})
	}
}

func TestAcceptedTaskErrorSuppressesNonBusinessClasses(t *testing.T) {
	for _, cause := range []error{ErrAuth, ErrTemporaryUpstream, context.Canceled} {
		err := acceptedTaskError("accepted-task", "poll", cause)
		if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) {
			t.Fatalf("acceptedTaskError(%v) = %v, want accepted temporary error", cause, err)
		}
		if errors.Is(err, ErrAuth) || errors.Is(err, context.Canceled) {
			t.Fatalf("acceptedTaskError(%v) retained a failover class: %v", cause, err)
		}
	}
}

func TestAcceptedTerminalBusinessErrorsRetainClass(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   error
	}{
		{status: "limit", want: ErrQuotaExhausted},
		{status: "risk", want: ErrRiskControl},
		{status: "invalid_param", want: ErrInvalidParams},
	} {
		t.Run(tc.status, func(t *testing.T) {
			var createCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					createCalls.Add(1)
					writeEnvelope(t, w, map[string]any{"parent_task_id": "accepted-task"})
				case "/api/inference/task/query_task_list":
					writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
						"status": tc.status, "fail_reason": "terminal business outcome",
					}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-2", Prompt: "test",
			})
			if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) || !errors.Is(err, tc.want) {
				t.Fatalf("accepted terminal %s error = %v, want marker, temporary, and %v", tc.status, err, tc.want)
			}
			if errors.Is(err, ErrAuth) {
				t.Fatalf("accepted terminal %s error retained auth: %v", tc.status, err)
			}
			if createCalls.Load() != 1 {
				t.Fatalf("create_task calls = %d, want 1", createCalls.Load())
			}
		})
	}
}

func TestAcceptedPollAuthAndQuotaErrorsCannotTriggerResubmission(t *testing.T) {
	for _, tc := range []struct {
		name         string
		httpStatus   int
		code         int
		message      string
		wantBusiness error
	}{
		{name: "auth", httpStatus: http.StatusUnauthorized, code: http.StatusUnauthorized, message: "session expired"},
		{name: "quota", httpStatus: http.StatusForbidden, code: 100000007, message: "credits exhausted", wantBusiness: ErrQuotaExhausted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var createCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					createCalls.Add(1)
					writeEnvelope(t, w, map[string]any{"parent_task_id": "accepted-task"})
				case "/api/inference/task/query_task_list":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.httpStatus)
					_ = json.NewEncoder(w).Encode(map[string]any{"code": tc.code, "message": tc.message})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-2", Prompt: "test",
			})
			if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) {
				t.Fatalf("post-accept %s error = %v, want ErrTaskAccepted and ErrTemporaryUpstream", tc.name, err)
			}
			if errors.Is(err, ErrAuth) {
				t.Fatalf("post-accept %s error retained auth: %v", tc.name, err)
			}
			if gotBusiness := errors.Is(err, ErrQuotaExhausted); gotBusiness != (tc.wantBusiness != nil) {
				t.Fatalf("post-accept %s quota classification = %v, want %v: %v", tc.name, gotBusiness, tc.wantBusiness != nil, err)
			}
			if createCalls.Load() != 1 {
				t.Fatalf("create_task calls = %d, want 1", createCalls.Load())
			}
		})
	}
}

func TestGenerateRetriesOnlyAcceptedTaskAssetURL(t *testing.T) {
	previousWait, previousPoll := maxGenerationWait, pollInterval
	maxGenerationWait, pollInterval = time.Second, 5*time.Millisecond
	t.Cleanup(func() { maxGenerationWait, pollInterval = previousWait, previousPoll })
	png := []byte("\x89PNG\r\n\x1a\nvalid")

	for _, tc := range []struct {
		name         string
		failures     int32
		wantAttempts int32
		wantError    bool
	}{
		{name: "transient", failures: 1, wantAttempts: 2},
		{name: "exhausted", failures: 10, wantAttempts: maxAssetDownloadAttempts, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var createCalls atomic.Int32
			var downloadCalls atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/user/current":
					writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
				case "/api/inference/v2/create_task":
					createCalls.Add(1)
					writeEnvelope(t, w, map[string]any{"parent_task_id": "accepted-task"})
				case "/api/inference/task/query_task_list":
					writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
						"status": "complete", "output": server.URL + "/api/resource-utils/accepted-task.png",
					}}})
				case "/api/resource-utils/accepted-task.png":
					if downloadCalls.Add(1) <= tc.failures {
						http.Error(w, "temporary CDN failure", http.StatusServiceUnavailable)
						return
					}
					w.Header().Set("Content-Type", "image/png")
					_, _ = w.Write(png)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			withAPIBase(t, server.URL+"/api")

			got, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
				Model: "lumina-nano-banana-pro", Prompt: "test", DownloadResult: true,
			})
			if tc.wantError {
				if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) {
					t.Fatalf("download error = %v, want ErrTaskAccepted and ErrTemporaryUpstream", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, png) {
					t.Fatalf("download bytes = %q, want PNG", got)
				}
			}
			if createCalls.Load() != 1 {
				t.Fatalf("create_task calls = %d, want 1", createCalls.Load())
			}
			if downloadCalls.Load() != tc.wantAttempts {
				t.Fatalf("asset download calls = %d, want %d", downloadCalls.Load(), tc.wantAttempts)
			}
		})
	}
}

func TestGenerateRejectsUntrustedResultURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/user/current":
			writeEnvelope(t, w, map[string]any{"user_id": "1", "role": "member"})
		case "/api/inference/v2/create_task":
			writeEnvelope(t, w, map[string]any{"parent_task_id": "task"})
		case "/api/inference/task/query_task_list":
			writeEnvelope(t, w, map[string]any{"tasks": []any{map[string]any{
				"status": "complete", "output": "https://evil.example/stolen.png",
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	_, _, err := NewClient("").GenerateImage(context.Background(), "csrfToken=x; sessionid=y", ImageRequest{
		Model: "lumina-nano-banana-2", Prompt: "test",
	})
	if !errors.Is(err, ErrTaskAccepted) || !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("untrusted result error = %v, want ErrTaskAccepted and ErrTemporaryUpstream", err)
	}
}

func TestOpenAssetCookieTrustImageValidationAndSizeLimit(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nvalid")
	var cookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie = r.Header.Get("Cookie")
		switch r.URL.Path {
		case "/api/resource-utils/image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(png)
		case "/api/not-image":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write([]byte("<html>not an image</html>"))
		case "/api/too-large":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(append(png, []byte("too much")...))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	client := NewClient("")

	raw, contentType, err := client.OpenAsset(context.Background(), "sessionid=secret", "/api/resource-utils/image")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, png) || contentType != "image/png" || cookie != "sessionid=secret" {
		t.Fatalf("asset bytes=%q type=%q cookie=%q", raw, contentType, cookie)
	}
	if _, _, err := client.OpenAsset(context.Background(), "sessionid=secret", "/not-image"); !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("non-image error = %v", err)
	}

	previousLimit := maxAssetBytes
	maxAssetBytes = int64(len(png))
	t.Cleanup(func() { maxAssetBytes = previousLimit })
	if _, _, err := client.OpenAsset(context.Background(), "sessionid=secret", "/too-large"); !errors.Is(err, ErrTemporaryUpstream) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestOpenAssetOnlyTreatsExplicitAPIAuthFailureAsAccountAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/session-expired":
			http.Error(w, `{"message":"session expired, please log in"}`, http.StatusUnauthorized)
		case "/api/asset-forbidden":
			http.Error(w, `{"message":"asset link forbidden"}`, http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	withAPIBase(t, server.URL+"/api")
	client := NewClient("")

	if _, _, err := client.OpenAsset(context.Background(), "sessionid=secret", "/session-expired"); !errors.Is(err, ErrAuth) {
		t.Fatalf("explicit session failure = %v, want ErrAuth", err)
	}
	if _, _, err := client.OpenAsset(context.Background(), "sessionid=secret", "/asset-forbidden"); !errors.Is(err, ErrTemporaryUpstream) || errors.Is(err, ErrAuth) {
		t.Fatalf("generic asset 403 = %v, want only ErrTemporaryUpstream", err)
	}
}

func TestAssetURLAllowlistAndCookieScope(t *testing.T) {
	withAPIBase(t, "https://lumi-api.console.byteplus.com/api")
	for _, value := range []string{
		"https://lumi-api.console.byteplus.com/api/resource-utils/a",
		"https://cdn.bytepluses.com/a.png", "https://a.bytepluscdn.com/a.png", "https://x.byteoversea.com/a.png",
	} {
		parsed, _ := url.Parse(value)
		if !allowedAssetURL(parsed) {
			t.Errorf("allowedAssetURL(%q) = false", value)
		}
	}
	for _, value := range []string{
		"http://cdn.bytepluses.com/a.png", "https://bytepluses.com.evil.example/a.png", "https://evil.example/a.png",
	} {
		parsed, _ := url.Parse(value)
		if allowedAssetURL(parsed) {
			t.Errorf("allowedAssetURL(%q) = true", value)
		}
	}
	for _, value := range []string{"//evil.example/a", "https://user@cdn.bytepluses.com/a.png"} {
		if _, err := normalizeAssetURL(value); !errors.Is(err, ErrInvalidParams) {
			t.Errorf("normalizeAssetURL(%q) error = %v", value, err)
		}
	}

	apiReq, _ := http.NewRequest(http.MethodGet, "https://lumi-api.console.byteplus.com/api/resource", nil)
	setAssetHeaders(apiReq, "sessionid=secret")
	if apiReq.Header.Get("Cookie") != "sessionid=secret" {
		t.Fatal("API-origin asset request did not receive its session cookie")
	}
	cdnReq, _ := http.NewRequest(http.MethodGet, "https://cdn.bytepluscdn.com/result.png", nil)
	cdnReq.Header.Set("Cookie", "copied-by-redirect")
	setAssetHeaders(cdnReq, "sessionid=secret")
	if got := cdnReq.Header.Get("Cookie"); got != "" {
		t.Fatalf("CDN asset request leaked Cookie %q", got)
	}
}
