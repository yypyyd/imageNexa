package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"backend/internal/http/middleware"
	"backend/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type V1Handler struct {
	v1 *service.V1Service
}

type imageSyncResult struct {
	response map[string]any
	err      error
}

func NewV1Handler(v1 *service.V1Service) *V1Handler {
	return &V1Handler{v1: v1}
}

// AdminTest restores the account-row capability probe. It is registered only
// inside the administrator session + CSRF group, never on the public /v1 API.
func (h *V1Handler) AdminTest(c *gin.Context) {
	var body struct {
		Model      string `json:"model"`
		Prompt     string `json:"prompt"`
		Ratio      string `json:"ratio"`
		Resolution string `json:"resolution"`
		Duration   string `json:"duration"`
		AccountID  string `json:"account_id"`
	}
	if !bindAdminJSON(c, &body, "invalid request body") {
		return
	}
	result, err := h.v1.PrepareAdminTest(c.Request.Context(), service.AdminTestRequest{
		Model: body.Model, Prompt: body.Prompt, AspectRatio: body.Ratio,
		Resolution: body.Resolution, Duration: body.Duration, AccountID: body.AccountID,
	})
	if err != nil {
		adminTestError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// AdminTestArtifact streams only artifacts created by an administrator test.
// The surrounding router group authenticates the admin session before this
// handler is reached; event ownership then rejects ordinary API-key artifacts.
func (h *V1Handler) AdminTestArtifact(c *gin.Context) {
	principal := &service.APIPrincipal{TokenType: "admin"}
	body, contentType, imageErr := h.v1.OpenImageContent(c.Request.Context(), principal, c.Param("id"))
	if imageErr != nil {
		body, contentType, imageErr = h.v1.OpenVideoContent(c.Request.Context(), principal, c.Param("id"))
	}
	if imageErr != nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": "test artifact not found"})
		return
	}
	defer body.Close()
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

// AdminArtifact restores the administrator log/gallery preview. The route is
// session protected and the service resolves the event's original API-key
// ownership internally, so no downstream bearer credential is exposed to the
// browser.
func (h *V1Handler) AdminArtifact(c *gin.Context) {
	body, contentType, err := h.v1.OpenAdminArtifact(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"detail": "artifact not found"})
		return
	}
	defer body.Close()
	c.Header("Content-Type", contentType)
	c.Header("Cache-Control", "private, no-store")
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

func adminTestError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, service.ErrUnknownModel):
		c.JSON(http.StatusNotFound, gin.H{"detail": "测试模型不存在或未启用"})
	case errors.Is(err, service.ErrUnsupportedParams):
		c.JSON(http.StatusBadRequest, gin.H{"detail": "测试参数不受该模型支持"})
	case errors.Is(err, service.ErrContentRejected), errors.Is(err, service.ErrBannedPrompt):
		c.JSON(http.StatusBadRequest, gin.H{"detail": "测试内容被拒绝"})
	case errors.Is(err, service.ErrProviderQuota), errors.Is(err, service.ErrInsufficientFunds):
		c.JSON(http.StatusTooManyRequests, gin.H{"detail": "所选账号额度不足"})
	case errors.Is(err, service.ErrProviderAuth):
		c.JSON(http.StatusBadGateway, gin.H{"detail": "所选账号凭据失效"})
	case errors.Is(err, service.ErrNoProviderAccount):
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "所选账号不能执行该模型"})
	case errors.Is(err, service.ErrProviderDisabled):
		c.JSON(http.StatusServiceUnavailable, gin.H{"detail": "该模型的所有平台均已在系统设置中停用"})
	default:
		c.JSON(http.StatusBadGateway, gin.H{"detail": "账号能力测试失败"})
	}
}

// apiPrincipal reuses the credential that RequireAPICredential already
// authenticated. Re-reading and hashing the bearer token in every handler
// doubled database traffic and created two independent auth decisions for one
// request.
func apiPrincipal(c *gin.Context) *service.APIPrincipal {
	credential := middleware.CurrentAPICredential(c)
	if credential == nil {
		return nil
	}
	return &service.APIPrincipal{Credential: credential, TokenType: "api_key"}
}

func (h *V1Handler) Models(c *gin.Context) {
	// The default response is strict OpenAI model-object shape. Capability
	// metadata is an opt-in 2API extension for clients that explicitly request
	// it with ?extended=true.
	extended := false
	if raw := strings.TrimSpace(c.Query("extended")); raw != "" {
		extended = strings.EqualFold(raw, "true") || raw == "1"
	}
	items, err := h.v1.ListModels(c.Request.Context(), extended)
	if err != nil {
		openaiError(c, http.StatusInternalServerError, "server_error", "", "failed to load models")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   items,
	})
}

// ChatCompletions — OpenAI POST /v1/chat/completions. The JSON request is
// forwarded to a configured custom OpenAI-compatible upstream; both ordinary
// JSON responses and SSE streaming are relayed without buffering.
func (h *V1Handler) ChatCompletions(c *gin.Context) {
	principal := apiPrincipal(c)
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 10<<20+1))
	if err != nil || len(body) > 10<<20 {
		openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "", "request body too large")
		return
	}
	resp, err := h.v1.PrepareChatCompletion(c.Request.Context(), principal, body)
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	defer resp.Body.Close()
	contentType := strings.TrimSpace(resp.Header.Get("Content-Type"))
	if resp.Stream {
		contentType = "text/event-stream"
	} else if contentType == "" {
		contentType = "application/json"
	}
	c.Header("Content-Type", contentType)
	if resp.Stream {
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("X-Accel-Buffering", "no")
	}
	for _, name := range []string{"OpenAI-Request-ID", "X-Request-ID", "OpenAI-Processing-Ms"} {
		if value := resp.Header.Get(name); value != "" {
			c.Header(name, value)
		}
	}
	c.Status(http.StatusOK)
	if resp.Stream {
		buf := make([]byte, 32<<10)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := c.Writer.Write(buf[:n]); writeErr != nil {
					return
				}
				c.Writer.Flush()
			}
			if readErr != nil {
				return
			}
		}
	}
	_, _ = io.Copy(c.Writer, resp.Body)
}

// ImageGenerations — OpenAI POST /v1/images/generations (text-to-image only).
// Accepts exactly OpenAI's fields. Size drives resolution for ordinary image
// models; quality selects a tier only for the GPT Image 2 family. Returns
// {created, data:[{url}]} by default.
func (h *V1Handler) ImageGenerations(c *gin.Context) {
	principal := apiPrincipal(c)
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageJSONBytes)

	var body struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		Quality        string `json:"quality"`
		ResponseFormat string `json:"response_format"`
		Background     string `json:"background"`
		OutputFormat   string `json:"output_format"`
		User           string `json:"user"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
			return
		}
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "invalid request body")
		return
	}

	h.respondImageRequest(c, principal, service.V1ImageRequest{
		Model:          body.Model,
		Prompt:         body.Prompt,
		RequestID:      imageRequestID(c),
		N:              body.N,
		Size:           body.Size,
		Quality:        body.Quality,
		ResponseFormat: body.ResponseFormat,
		Background:     body.Background,
		OutputFormat:   body.OutputFormat,
		BaseURL:        requestBaseURL(c),
	})
}

// ImageEdits — OpenAI POST /v1/images/edits (image-to-image). multipart/form-data
// only: image / image[] file uploads, prompt, model, n, size, quality. Masks are
// rejected because no retained provider can preserve mask semantics. Files
// become reference images. Returns {created, data:[{url}]} by default.
func (h *V1Handler) ImageEdits(c *gin.Context) {
	principal := apiPrincipal(c)
	if !strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "images/edits requires multipart/form-data")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageEditMultipartBytes)
	defer cleanupMultipartForm(c.Request)
	if err := c.Request.ParseMultipartForm(16 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "multipart body too large")
			return
		}
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "invalid multipart form")
		return
	}
	if len(c.Request.MultipartForm.File["mask"]) > 0 {
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "unsupported_parameter", "mask is not supported")
		return
	}
	refs, readErr := readMultipartImagesStrict(c, "image", "image[]")
	if readErr != nil {
		status := http.StatusBadRequest
		if errors.Is(readErr, errReferenceMediaTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		openaiError(c, status, "invalid_request_error", "", readErr.Error())
		return
	}
	if len(refs) == 0 {
		openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "images/edits requires at least one image file")
		return
	}
	n, _ := strconv.Atoi(strings.TrimSpace(c.PostForm("n")))
	h.respondImageRequest(c, principal, service.V1ImageRequest{
		Model:           c.PostForm("model"),
		Prompt:          c.PostForm("prompt"),
		RequestID:       imageRequestID(c),
		N:               n,
		Size:            c.PostForm("size"),
		Quality:         c.PostForm("quality"),
		ResponseFormat:  c.PostForm("response_format"),
		Background:      c.PostForm("background"),
		OutputFormat:    c.PostForm("output_format"),
		ReferenceImages: refs,
		ReferenceGrid:   parseFormBool(c.PostForm("reference_grid")),
		BaseURL:         requestBaseURL(c),
	})
}

func (h *V1Handler) respondImageRequest(c *gin.Context, principal *service.APIPrincipal, request service.V1ImageRequest) {
	// Every public image request gets a durable request ID, including synchronous
	// calls whose upstream task can outlive the HTTP request. If a non-idempotent
	// provider accepted work before a timeout, callers can use these headers to
	// resume the original task instead of submitting (and paying for) it twice.
	request.RequestID = ensureImageRequestID(c, request.RequestID)
	c.Header("Idempotency-Key", request.RequestID)
	c.Header("Location", imageTaskPollURL(c, request.RequestID))
	if !imageAsyncRequested(c) {
		resp, err := h.v1.PrepareImageRequest(c.Request.Context(), principal, request)
		h.finishSynchronousImageResponse(c, imageSyncResult{response: resp, err: err})
		return
	}

	resp, pending, err := h.v1.StartImageRequest(c.Request.Context(), principal, request)
	if err != nil {
		h.writeV1Error(c, err, resp)
		return
	}
	c.Header("Idempotency-Key", request.RequestID)
	if pollURL, _ := resp["poll_url"].(string); pollURL != "" {
		c.Header("Location", pollURL)
	}
	if pending {
		c.Header("Preference-Applied", "respond-async")
		c.Header("Retry-After", "3")
		c.JSON(http.StatusAccepted, resp)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func imageTaskPollURL(_ *gin.Context, requestID string) string {
	path := "/v1/images/tasks?request_id=" + url.QueryEscape(strings.TrimSpace(requestID))
	return path
}

// Synchronous requests do not commit headers until generation finishes. Long
// callers should opt into Prefer: respond-async; emitting whitespace heartbeats
// would lock failures to HTTP 200 and break OpenAI SDK error handling.
func (h *V1Handler) finishSynchronousImageResponse(c *gin.Context, result imageSyncResult) {
	if result.err == nil {
		c.JSON(http.StatusOK, openaiImageResponse(result.response))
		return
	}
	if errors.Is(result.err, service.ErrGenerationAccepted) {
		c.Header("Preference-Applied", "respond-async")
		c.Header("Retry-After", "3")
		c.JSON(http.StatusAccepted, result.response)
		return
	}

	status, body := v1ErrorResponse(result.err, result.response)
	c.JSON(status, body)
}

func (h *V1Handler) GetImageTask(c *gin.Context) {
	principal := apiPrincipal(c)
	var err error
	resp, err := h.v1.ImageTask(c.Request.Context(), principal, c.Query("request_id"))
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func imageRequestID(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("Idempotency-Key")); value != "" {
		return value
	}
	return strings.TrimSpace(c.GetHeader("X-Request-ID"))
}

func ensureImageRequestID(c *gin.Context, requestID string) string {
	if value := strings.TrimSpace(requestID); value != "" {
		return value
	}
	if value := strings.TrimSpace(c.Writer.Header().Get("X-Request-Id")); value != "" {
		return value
	}
	return uuid.NewString()
}

func imageAsyncRequested(c *gin.Context) bool {
	prefer := strings.ToLower(c.GetHeader("Prefer"))
	for _, item := range strings.Split(prefer, ",") {
		if strings.TrimSpace(strings.SplitN(item, ";", 2)[0]) == "respond-async" {
			return true
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.Query("async"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CreateVideo — OpenAI POST /v1/videos. Creates an async job and returns the
// video object immediately ({id, status:"queued"}). Accepts JSON {model, prompt,
// seconds, size} or multipart (with an input_reference file). size→ratio+
// resolution, seconds→duration.
func (h *V1Handler) CreateVideo(c *gin.Context) {
	principal := apiPrincipal(c)
	var err error
	var modelID, prompt, seconds, size, resolutionOverride string
	var refs []string
	var videos, audios []service.MediaReference
	var generateAudio bool
	var referenceGrid bool
	if c.Request.ContentLength > maxGenerateMultipartBytes {
		openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxGenerateMultipartBytes)
	if strings.HasPrefix(c.GetHeader("Content-Type"), "multipart/form-data") {
		defer cleanupMultipartForm(c.Request)
		if err := c.Request.ParseMultipartForm(32 << 20); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "multipart body too large")
				return
			}
			openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "invalid multipart form")
			return
		}
		modelID = c.PostForm("model")
		prompt = c.PostForm("prompt")
		seconds = c.PostForm("seconds")
		size = c.PostForm("size")
		resolutionOverride = c.PostForm("resolution")
		var readErr error
		refs, readErr = readMultipartImagesStrict(c, "input_reference", "input_reference[]", "reference_images", "reference_images[]")
		if readErr == nil {
			videos, readErr = readMultipartMedia(c, 200<<20, "video", "reference_videos", "reference_videos[]", "input_video")
		}
		if readErr == nil {
			audios, readErr = readMultipartMedia(c, 50<<20, "audio", "reference_audios", "reference_audios[]", "input_audio")
		}
		if readErr != nil {
			status := http.StatusBadRequest
			code := ""
			message := readErr.Error()
			if errors.Is(readErr, errReferenceMediaTooLarge) {
				status = http.StatusRequestEntityTooLarge
				code = "request_too_large"
				message = "reference media is too large"
			}
			openaiError(c, status, "invalid_request_error", code, message)
			return
		}
		generateAudio = parseFormBool(c.PostForm("generate_audio"))
		referenceGrid = parseFormBool(c.PostForm("reference_grid"))
	} else {
		var body struct {
			Model   string          `json:"model"`
			Prompt  string          `json:"prompt"`
			Seconds json.RawMessage `json:"seconds"`
			Size    string          `json:"size"`
			// Resolution is a gateway extension for provider tiers that OpenAI's
			// standard 720p/1080p size mapping cannot express (for example 480p).
			Resolution string `json:"resolution"`
			// Reference frames (image-to-video / first-last frames) as base64 or
			// data-URI strings — the JSON equivalent of multipart input_reference.
			InputReference  []string `json:"input_reference"`
			ReferenceImages []string `json:"reference_images"`
			ReferenceVideos []string `json:"reference_videos"`
			ReferenceAudios []string `json:"reference_audios"`
			GenerateAudio   bool     `json:"generate_audio"`
			ReferenceGrid   bool     `json:"reference_grid"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				openaiError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body too large")
				return
			}
			openaiError(c, http.StatusBadRequest, "invalid_request_error", "", "invalid request body")
			return
		}
		modelID, prompt, size = body.Model, body.Prompt, body.Size
		resolutionOverride = body.Resolution
		seconds = rawToString(body.Seconds)
		imageInputs := make([]string, 0, len(body.InputReference)+len(body.ReferenceImages))
		imageInputs = append(imageInputs, body.InputReference...)
		imageInputs = append(imageInputs, body.ReferenceImages...)
		refs, err = decodeJSONImages(imageInputs)
		if err == nil {
			videos, err = decodeJSONMedia(body.ReferenceVideos, "video/mp4")
		}
		if err == nil {
			audios, err = decodeJSONMedia(body.ReferenceAudios, "audio/mpeg")
		}
		if err != nil {
			status := http.StatusBadRequest
			code := ""
			message := err.Error()
			if errors.Is(err, errReferenceMediaTooLarge) {
				status = http.StatusRequestEntityTooLarge
				code = "request_too_large"
				message = "reference media is too large"
			}
			openaiError(c, status, "invalid_request_error", code, message)
			return
		}
		generateAudio = body.GenerateAudio
		referenceGrid = body.ReferenceGrid
	}
	duration := strings.TrimSpace(seconds)
	if duration != "" && !strings.HasSuffix(duration, "s") {
		duration += "s"
	}
	aspect, resolution := videoSizeToInternal(size)
	if override := normalizeVideoResolutionOverride(resolutionOverride); override != "" {
		resolution = override
	}
	requestID := ensureImageRequestID(c, imageRequestID(c))
	resp, err := h.v1.StartVideoJob(c.Request.Context(), principal, service.V1VideoRequest{
		Model:           modelID,
		Prompt:          prompt,
		RequestID:       requestID,
		Duration:        duration,
		AspectRatio:     aspect,
		Resolution:      resolution,
		ReferenceImages: refs,
		ReferenceVideos: videos,
		ReferenceAudios: audios,
		ReferenceGrid:   referenceGrid,
		GenerateAudio:   generateAudio,
		BaseURL:         requestBaseURL(c),
	})
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	c.Header("Idempotency-Key", requestID)
	c.JSON(http.StatusOK, resp)
}

func normalizeVideoResolutionOverride(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	if strings.HasSuffix(value, "p") {
		return value
	}
	if _, err := strconv.Atoi(value); err == nil {
		return value + "p"
	}
	return value
}

// GetVideo — OpenAI GET /v1/videos/{id}. Returns the job status object.
func (h *V1Handler) GetVideo(c *gin.Context) {
	principal := apiPrincipal(c)
	var err error
	resp, err := h.v1.VideoJob(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	c.JSON(http.StatusOK, resp)
}

// GetVideoContent — OpenAI GET /v1/videos/{id}/content. Streams the rendered mp4
// by proxying the stored upstream URL (downloaded on demand, never persisted).
func (h *V1Handler) GetVideoContent(c *gin.Context) {
	principal := apiPrincipal(c)
	var err error
	body, contentType, err := h.v1.OpenVideoContent(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	defer body.Close()
	c.Header("Content-Type", contentType)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

// GetImageContent — GET /v1/images/{id}/content. The surrounding /v1 group
// authenticates the bearer key, and OpenImageContent enforces that the image
// belongs to that credential.
func (h *V1Handler) GetImageContent(c *gin.Context) {
	principal := apiPrincipal(c)
	body, contentType, err := h.v1.OpenImageContent(c.Request.Context(), principal, c.Param("id"))
	if err != nil {
		h.writeV1Error(c, err, nil)
		return
	}
	defer body.Close()
	c.Header("Content-Type", contentType)
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

// rawToString accepts OpenAI's `seconds` whether sent as a JSON string or number.
func rawToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return strings.Trim(string(raw), `"`)
}

// videoSizeToInternal maps OpenAI's "WxH" size to our aspect ratio + resolution
// tier (height ≥1080 → 1080p, else 720p).
func videoSizeToInternal(size string) (ratio, resolution string) {
	var w, h int
	if s := strings.TrimSpace(strings.ToLower(size)); s != "" {
		_, _ = fmt.Sscanf(s, "%dx%d", &w, &h)
	}
	if w == 0 || h == 0 {
		return "16:9", "720p"
	}
	// The "p" resolution is the SHORT edge (720p = 1280×720, 1080p = 1920×1080),
	// so a standard 1280×720 must read as 720p — not 1080p off the long edge.
	resolution = "720p"
	if min(w, h) >= 1080 {
		resolution = "1080p"
	}
	return guessRatioWH(w, h), resolution
}

func guessRatioWH(w, h int) string {
	if w == h {
		return "1:1"
	}
	r := float64(w) / float64(h)
	cands := []struct {
		name string
		v    float64
	}{{"16:9", 16.0 / 9}, {"9:16", 9.0 / 16}, {"4:3", 4.0 / 3}, {"3:4", 3.0 / 4}, {"1:1", 1}}
	best, bestD := "16:9", 1e9
	for _, cd := range cands {
		d := r - cd.v
		if d < 0 {
			d = -d
		}
		if d < bestD {
			best, bestD = cd.name, d
		}
	}
	return best
}

// openaiImageResponse strips our rich internal map down to OpenAI's image shape.
func openaiImageResponse(m map[string]any) gin.H {
	out := gin.H{"created": m["created"]}
	if d, ok := m["data"]; ok && d != nil {
		out["data"] = d
	} else {
		out["data"] = []any{}
	}
	return out
}

// openaiError writes an OpenAI-format error body:
// {"error": {"message", "type", "param", "code"}}.
func openaiError(c *gin.Context, status int, errType, code, message string) {
	c.JSON(status, openaiErrorResponse(errType, code, message))
}

func openaiErrorResponse(errType, code, message string) gin.H {
	body := gin.H{"message": message, "type": errType, "param": nil, "code": nil}
	if code != "" {
		body["code"] = code
	}
	return gin.H{"error": body}
}

func (h *V1Handler) writeV1Error(c *gin.Context, err error, payload map[string]any) {
	status, body := v1ErrorResponse(err, payload)
	c.JSON(status, body)
}

func v1ErrorResponse(err error, payload map[string]any) (int, any) {
	switch {
	case errors.Is(err, service.ErrIdempotencyConflict):
		return http.StatusConflict, openaiErrorResponse("invalid_request_error", "idempotency_key_conflict", "Idempotency key was already used with a different request.")
	case errors.Is(err, service.ErrUnknownModel):
		return http.StatusNotFound, openaiErrorResponse("invalid_request_error", "model_not_found", "The requested model does not exist.")
	case errors.Is(err, service.ErrUnsupportedParams):
		return http.StatusBadRequest, openaiErrorResponse("invalid_request_error", "", "The request contains unsupported parameters.")
	case errors.Is(err, service.ErrBannedPrompt):
		return http.StatusBadRequest, openaiErrorResponse("invalid_request_error", "content_policy_violation", "The prompt violates the configured content policy.")
	case errors.Is(err, service.ErrContentRejected):
		return http.StatusBadRequest, openaiErrorResponse("invalid_request_error", "content_policy_violation", "The request was rejected by the upstream content policy.")
	case errors.Is(err, service.ErrInsufficientFunds):
		return http.StatusPaymentRequired, openaiErrorResponse("insufficient_quota", "insufficient_quota", "Insufficient quota.")
	case errors.Is(err, service.ErrReferenceTooLarge), errors.Is(err, service.ErrReferenceVideoTooLarge), errors.Is(err, service.ErrReferenceAudioTooLarge):
		return http.StatusRequestEntityTooLarge, openaiErrorResponse("invalid_request_error", "request_too_large", "Reference media is too large.")
	case errors.Is(err, service.ErrNoProviderAccount):
		return http.StatusServiceUnavailable, openaiErrorResponse("server_error", "", "No upstream provider account is currently available.")
	case errors.Is(err, service.ErrProviderDisabled):
		return http.StatusServiceUnavailable, openaiErrorResponse("server_error", "provider_disabled", "All upstream providers for this model are currently disabled.")
	case errors.Is(err, service.ErrProviderAuth):
		return http.StatusServiceUnavailable, openaiErrorResponse("server_error", "", "Upstream provider authentication failed.")
	case errors.Is(err, service.ErrProviderQuota):
		return http.StatusTooManyRequests, openaiErrorResponse("insufficient_quota", "insufficient_quota", "Upstream provider quota is exhausted.")
	case errors.Is(err, service.ErrProviderTemporary):
		return http.StatusServiceUnavailable, openaiErrorResponse("server_error", "", "Upstream provider is temporarily unavailable.")
	case errors.Is(err, service.ErrImageTaskNotFound):
		return http.StatusNotFound, openaiErrorResponse("invalid_request_error", "not_found", "Image task not found.")
	case errors.Is(err, service.ErrConcurrencyBackendUnavailable):
		return http.StatusServiceUnavailable, openaiErrorResponse("server_error", "concurrency_backend_unavailable", "Concurrency control is temporarily unavailable.")
	case errors.Is(err, service.ErrConcurrencyFull), errors.Is(err, service.ErrUserConcurrencyFull):
		return http.StatusTooManyRequests, openaiErrorResponse("rate_limit_error", "rate_limit_exceeded", "Too many generations are currently in progress.")
	case errors.Is(err, service.ErrVideoJobNotFound):
		return http.StatusNotFound, openaiErrorResponse("invalid_request_error", "not_found", "Video job not found.")
	case errors.Is(err, service.ErrVideoNotReady):
		return http.StatusConflict, openaiErrorResponse("invalid_request_error", "", "Video is not ready yet.")
	case errors.Is(err, service.ErrProviderUnsupported):
		return http.StatusNotImplemented, openaiErrorResponse("server_error", "", "The requested operation is not supported by an available provider.")
	case errors.Is(err, service.ErrProviderExecution):
		return http.StatusBadGateway, openaiErrorResponse("server_error", "", "Upstream provider request failed.")
	case errors.Is(err, service.ErrGenerationPending):
		return http.StatusNotImplemented, payload
	default:
		return http.StatusInternalServerError, openaiErrorResponse("server_error", "", "The server encountered an internal error.")
	}
}

// requestBaseURL is a development fallback for service-generated URLs. In
// production Config.PublicBaseURL is authoritative. Forwarded headers are not
// consulted here because they are client-controlled unless every proxy hop is
// authenticated; the bundled proxy overwrites them and Location stays relative.
func requestBaseURL(c *gin.Context) string {
	host := strings.TrimSpace(c.Request.Host)
	if host == "" || strings.ContainsAny(host, "/\\@?#") {
		return ""
	}
	parsed, err := url.Parse("http://" + host)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	if port := parsed.Port(); port != "" {
		n, parseErr := strconv.Atoi(port)
		if parseErr != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + parsed.Host
}

func cleanupMultipartForm(request *http.Request) {
	if request != nil && request.MultipartForm != nil {
		_ = request.MultipartForm.RemoveAll()
	}
}
