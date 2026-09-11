package byteplus

import (
	"bytes"
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"backend/internal/netguard"
)

const (
	defaultImageXEndpoint = "https://imagex-ap-southeast-1.bytevcloudapi.com/"
	imageXRegion          = "ap-southeast-1"
	imageXService         = "imagex"
	imageXServiceID       = "3rqcfx17W1"

	maxReferenceBytes        = 25 << 20
	defaultMaxAssetBytes     = 128 << 20
	defaultCreateTaskTimeout = 90 * time.Second
	defaultMaxGenerationWait = 10 * time.Minute
	maxAssetDownloadAttempts = 3
)

var (
	imageXEndpointURL       = defaultImageXEndpoint
	pollInterval            = time.Second
	createTaskTimeout       = defaultCreateTaskTimeout
	maxGenerationWait       = defaultMaxGenerationWait
	maxAssetBytes     int64 = defaultMaxAssetBytes
)

// ImageRequest is the provider-neutral subset needed by Lumina's five image
// models. Size accepts either an exact WxH or a 1K/2K/4K tier; the provider
// translates it to each model's actual schema.
type ImageRequest struct {
	Model  string
	Prompt string
	Size   string
	// Resolution is the provider tier (1K/2K/4K). When present it is
	// authoritative over Size, whose OpenAI-compatible value is often WxH.
	Resolution     string
	AspectRatio    string
	Quality        string
	Seed           int64
	References     [][]byte
	DownloadResult bool
}

type inferenceInput struct {
	Format       string `json:"format"`
	Name         string `json:"name"`
	InternalName string `json:"internal_name"`
	Label        string `json:"label"`
	Type         string `json:"type,omitempty"`
	Value        string `json:"value"`
}

type createTaskPayload struct {
	Inputs          []inferenceInput `json:"inputs"`
	InferenceConfig struct {
		InferencePipeline string `json:"inference_pipeline"`
		InferenceID       string `json:"inference_id"`
		InferenceVerID    string `json:"inference_ver_id"`
		Name              string `json:"name"`
		ReqKey            string `json:"req_key"`
	} `json:"inference_config"`
	InferenceType string `json:"inference_type"`
	RequestSource int    `json:"request_source"`
	Count         int    `json:"count"`
}

// GenerateImage uploads optional references, submits a Lumina task, polls its
// parent task to a terminal state, and optionally downloads the first successful
// image. URL-only mode still returns image_url in metadata for the authenticated
// OpenAsset path.
func (c *Client) GenerateImage(ctx context.Context, cookie string, request ImageRequest) ([]byte, map[string]any, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" || CSRFTokenFromCookie(cookie) == "" {
		return nil, nil, ErrAuth
	}
	spec, ok := LookupModel(request.Model)
	if !ok {
		return nil, nil, fmt.Errorf("%w: unsupported model %q", ErrInvalidParams, request.Model)
	}
	request.Prompt = strings.TrimSpace(request.Prompt)
	if request.Prompt == "" {
		return nil, nil, fmt.Errorf("%w: prompt is required", ErrInvalidParams)
	}
	if len(request.References) > spec.MaxReferences {
		return nil, nil, fmt.Errorf("%w: model accepts at most %d reference images", ErrInvalidParams, spec.MaxReferences)
	}

	profile, err := c.FetchProfile(ctx, cookie)
	if err != nil {
		return nil, nil, err
	}
	userID := strings.TrimSpace(stringValue(profile["user_id"]))
	userName := strings.TrimSpace(stringValue(profile["user_name"]))
	if len(request.References) > 0 && userName == "" {
		return nil, nil, fmt.Errorf("%w: profile is missing upload user name", ErrTemporaryUpstream)
	}

	refURIs := make([]string, 0, len(request.References))
	var uploadToken *imageXToken
	for index, raw := range request.References {
		if len(raw) == 0 {
			return nil, nil, fmt.Errorf("%w: reference image %d is empty", ErrInvalidParams, index+1)
		}
		if len(raw) > maxReferenceBytes {
			return nil, nil, fmt.Errorf("%w: reference image %d exceeds %d MiB", ErrInvalidParams, index+1, maxReferenceBytes>>20)
		}
		if uploadToken == nil {
			uploadToken, err = c.fetchImageXToken(ctx, cookie)
			if err != nil {
				return nil, nil, err
			}
		}
		uri, uploadErr := c.uploadImageX(ctx, cookie, *uploadToken, userID, userName, index, raw)
		if uploadErr != nil {
			return nil, nil, uploadErr
		}
		refURIs = append(refURIs, uri)
	}

	payload, err := buildCreateTaskPayload(spec, request, refURIs)
	if err != nil {
		return nil, nil, err
	}
	createCtx, cancelCreate := context.WithTimeout(ctx, createTaskTimeout)
	created, err := c.apiData(createCtx, http.MethodPost, "/inference/v2/create_task", cookie, payload)
	cancelCreate()
	if err != nil {
		// Only a lost answer (transport failure, timeout, 5xx, unparseable body)
		// is ambiguous. A 4xx or a business error code is Lumina refusing the
		// request, so no task exists and the caller may fail over normally.
		if errors.Is(err, ErrTemporaryUpstream) && !errors.Is(err, ErrUpstreamRejected) {
			return nil, nil, taskSubmissionUnknownError(err)
		}
		return nil, nil, err
	}
	parentTaskID := extractTaskID(created)
	if parentTaskID == "" {
		return nil, nil, taskSubmissionUnknownError(errors.New("response missing parent_task_id"))
	}

	// From this point onward create_task has been accepted and must never be
	// repeated. Keep the post-acceptance path in ResumeImageTask so a process
	// restart or a later /v1/images/tasks poll can continue this exact task id
	// without replaying the non-idempotent submission.
	asset, meta, err := c.ResumeImageTask(ctx, cookie, parentTaskID, request.DownloadResult)
	if meta != nil {
		meta["model"] = spec.Key
		meta["model_id"] = spec.ID
	}
	return asset, meta, err
}

// ResumeImageTask continues an already accepted Lumina parent task. It never
// uploads references and never calls create_task, which makes it safe to invoke
// after a timeout or process restart when the durable parent task id is known.
func (c *Client) ResumeImageTask(ctx context.Context, cookie, taskID string, downloadResult bool) ([]byte, map[string]any, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" || CSRFTokenFromCookie(cookie) == "" {
		return nil, nil, ErrAuth
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, nil, fmt.Errorf("%w: accepted task id is required", ErrInvalidParams)
	}

	acceptedCtx, cancelAccepted := context.WithTimeout(ctx, maxGenerationWait)
	defer cancelAccepted()
	task, imageURL, contentType, status, err := c.pollImageTask(acceptedCtx, cookie, taskID)
	if err != nil {
		return nil, nil, acceptedTaskError(taskID, "poll", err)
	}
	imageURL, err = normalizeAssetURL(imageURL)
	if err != nil {
		return nil, nil, acceptedTaskError(taskID, "validate result URL", err)
	}
	meta := map[string]any{
		"provider":     "byteplus",
		"task_id":      taskID,
		"status":       status,
		"image_url":    imageURL,
		"content_type": contentType,
	}
	if inferenceInfo, ok := task["inference_info"]; ok {
		meta["inference_info"] = inferenceInfo
	}
	if !downloadResult {
		return nil, meta, nil
	}
	asset, detectedType, err := c.openAcceptedAsset(acceptedCtx, cookie, imageURL)
	if err != nil {
		return nil, nil, acceptedTaskError(taskID, "download result", err)
	}
	if detectedType != "" {
		meta["content_type"] = detectedType
	}
	return asset, meta, nil
}

func taskSubmissionUnknownError(cause error) error {
	// Keep only the no-resubmit/temporary classification. Explicit auth, quota,
	// risk, and invalid-parameter responses never reach this helper.
	return fmt.Errorf("%w: create_task: %v", ErrTaskSubmissionUnknown, cause)
}

// SubmittedTaskQuery identifies an ambiguous create_task call by what was sent
// and when it could have been accepted. Model accepts any LookupModel alias.
type SubmittedTaskQuery struct {
	Model  string
	Prompt string
	From   time.Time
	To     time.Time
}

// maxTaskHistoryPages bounds the history walk. The verification window is
// minutes wide and the history is newest-first, so a handful of pages is ample.
const maxTaskHistoryPages = 5

// FindSubmittedTask searches the account's Lumina task history for the parent
// task of an ambiguous create_task call. It is the only safe way to learn
// whether a lost response actually created a task, because create_task itself
// must never be replayed. It returns the matching parent task id, or "" when the
// history was read successfully and no task in the window matches. Any error
// means the history could not be read and the outcome remains unknown.
func (c *Client) FindSubmittedTask(ctx context.Context, cookie string, query SubmittedTaskQuery) (string, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" || CSRFTokenFromCookie(cookie) == "" {
		return "", ErrAuth
	}
	spec, ok := LookupModel(query.Model)
	if !ok {
		return "", fmt.Errorf("%w: unsupported model %q", ErrInvalidParams, query.Model)
	}
	if query.From.IsZero() || query.To.IsZero() || !query.To.After(query.From) {
		return "", fmt.Errorf("%w: task window is required", ErrInvalidParams)
	}
	// An expired Lumina session degrades to a code-0 guest profile whose task
	// history is legitimately empty. Only an authenticated read may serve as
	// evidence of absence, so the session is validated the same way create_task
	// validates it.
	if _, err := c.FetchProfile(ctx, cookie); err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(query.Prompt)
	const pageSize = 20
	for page := 1; page <= maxTaskHistoryPages; page++ {
		data, err := c.apiData(ctx, http.MethodPost, "/inference/task/query_task_list", cookie, map[string]any{
			"page_num": page, "page_size": pageSize, "source": []any{},
		})
		if err != nil {
			return "", err
		}
		root, _ := data.(map[string]any)
		tasks, _ := root["tasks"].([]any)
		if len(tasks) == 0 {
			return "", nil
		}
		reachedWindowStart := false
		for _, raw := range tasks {
			task, _ := raw.(map[string]any)
			createdAt, known := taskCreatedAt(task)
			if !known {
				continue
			}
			if createdAt.Before(query.From) {
				reachedWindowStart = true
				continue
			}
			if createdAt.After(query.To) || taskInferenceID(task) != spec.ID {
				continue
			}
			if taskPrompt, found := taskPromptInput(task); prompt != "" && found && taskPrompt != prompt {
				continue
			}
			if id := strings.TrimSpace(stringValue(task["id"])); id != "" {
				return id, nil
			}
		}
		if reachedWindowStart || len(tasks) < pageSize {
			return "", nil
		}
	}
	return "", fmt.Errorf("%w: task history window not reached", ErrTemporaryUpstream)
}

func taskCreatedAt(task map[string]any) (time.Time, bool) {
	for _, key := range []string{"created_at", "create_time", "created"} {
		raw, exists := task[key]
		if !exists {
			continue
		}
		value, ok := numberValue(raw)
		if !ok || value <= 0 {
			continue
		}
		// Lumina reports millisecond epochs; tolerate second epochs as well.
		if value > 1e12 {
			return time.UnixMilli(int64(value)), true
		}
		return time.Unix(int64(value), 0), true
	}
	return time.Time{}, false
}

func taskInferenceID(task map[string]any) string {
	if info, ok := task["inference_info"].(map[string]any); ok {
		for _, key := range []string{"inference_id", "inference_version_id"} {
			if value := strings.TrimSpace(stringValue(info[key])); value != "" {
				return value
			}
		}
	}
	if value := strings.TrimSpace(stringValue(task["inference_id"])); value != "" {
		return value
	}
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		if value := strings.TrimSpace(stringValue(child["model_id"])); value != "" {
			return value
		}
	}
	return ""
}

// taskPromptInput returns the prompt the task was created with. Lumina keeps
// the resolved inputs on each child; the parent's inference_config is a JSON
// string with the same shape and serves as the fallback.
func taskPromptInput(task map[string]any) (string, bool) {
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		if prompt, ok := promptFromInputs(child["inputs"]); ok {
			return prompt, true
		}
	}
	switch config := task["inference_config"].(type) {
	case map[string]any:
		return promptFromInputs(config["inputs"])
	case string:
		var decoded map[string]any
		if json.Unmarshal([]byte(config), &decoded) == nil {
			return promptFromInputs(decoded["inputs"])
		}
	}
	return "", false
}

func promptFromInputs(raw any) (string, bool) {
	inputs, _ := raw.([]any)
	for _, rawInput := range inputs {
		input, _ := rawInput.(map[string]any)
		name := strings.TrimSpace(stringValue(input["name"]))
		internal := strings.TrimSpace(stringValue(input["internal_name"]))
		if name == "prompt" || internal == "prompt" {
			return strings.TrimSpace(stringValue(input["value"])), true
		}
	}
	return "", false
}

type acceptedTaskFailure struct {
	message  string
	business error
	taskID   string
}

func (failure *acceptedTaskFailure) Error() string {
	return failure.message
}

func (failure *acceptedTaskFailure) Unwrap() []error {
	causes := []error{ErrTaskAccepted}
	if failure.business != nil {
		causes = append(causes, failure.business)
	}
	return causes
}

func acceptedTaskError(taskID, stage string, cause error) error {
	// Auth, generic temporary, and context errors remain diagnostic text only:
	// after acceptance they cannot prove that the durable account is bad. Explicit
	// terminal business outcomes retain their class for account state and public
	// error mapping, while ErrTaskAccepted still prevents any resubmission.
	var business error
	for _, candidate := range []error{ErrQuotaExhausted, ErrRiskControl, ErrInvalidParams, ErrRetryableTaskFailed, ErrTaskFailed} {
		if errors.Is(cause, candidate) {
			business = candidate
			break
		}
	}
	return &acceptedTaskFailure{
		message:  fmt.Sprintf("%s: %s for task %s: %v", ErrTaskAccepted, stage, taskID, cause),
		business: business,
		taskID:   strings.TrimSpace(taskID),
	}
}

// AcceptedTaskID returns the durable upstream identifier only when create_task
// definitely succeeded. Callers must not scrape Error() text because messages
// are sanitized and may change independently of recovery state.
func AcceptedTaskID(err error) string {
	var failure *acceptedTaskFailure
	if errors.As(err, &failure) {
		return failure.taskID
	}
	return ""
}

func buildCreateTaskPayload(spec ModelSpec, request ImageRequest, refs []string) (createTaskPayload, error) {
	var payload createTaskPayload
	if spec.MaxReferences < 0 || len(refs) > spec.MaxReferences {
		return payload, fmt.Errorf("%w: model accepts at most %d reference images", ErrInvalidParams, spec.MaxReferences)
	}
	payload.InferenceConfig.InferencePipeline = "vproxy_overpass"
	payload.InferenceConfig.InferenceID = spec.ID
	payload.InferenceConfig.InferenceVerID = spec.ID
	payload.InferenceConfig.Name = spec.Name
	payload.InferenceConfig.ReqKey = spec.ReqKey
	payload.InferenceType = "t2i"
	if len(refs) > 0 {
		payload.InferenceType = "i2i"
	}
	payload.RequestSource = 1
	payload.Count = 1
	effectiveSize := strings.TrimSpace(request.Resolution)
	if effectiveSize == "" {
		effectiveSize = request.Size
	}

	add := func(format, name, internalName, value string) {
		payload.Inputs = append(payload.Inputs, inferenceInput{
			Format: format, Name: name, InternalName: internalName, Label: name, Value: value,
		})
	}
	add("text_area", "prompt", "prompt", request.Prompt)
	seed := request.Seed
	if seed <= 0 {
		seed = -1
	}
	if seed > math.MaxInt32 {
		return payload, fmt.Errorf("%w: seed must be between -1 and %d", ErrInvalidParams, math.MaxInt32)
	}

	switch spec.Key {
	case ModelSeedream50Pro:
		size, err := seedreamProSize(effectiveSize, request.AspectRatio)
		if err != nil {
			return payload, err
		}
		add("input_number", "min_ratio", "inner_min_ratio", "0.07")
		add("input_number", "max_ratio", "inner_max_ratio", "16")
		add("select", "cot_mode", "optimize_prompt_options.thinking", "enabled")
		add("input_boolean", "use_pre_llm", "optimize_prompt", "true")
		add("custom", "size", "size", size)
		add("input_number", "seed", "seed", strconv.FormatInt(seed, 10))

	case ModelGPTImage2:
		quality := strings.ToLower(strings.TrimSpace(request.Quality))
		if quality == "" {
			quality = "low"
		}
		if !oneOf(quality, "low", "medium", "high") {
			return payload, fmt.Errorf("%w: unsupported GPT Image 2 quality %q", ErrInvalidParams, request.Quality)
		}
		size, err := gptImageSize(effectiveSize, request.AspectRatio)
		if err != nil {
			return payload, err
		}
		add("select", "quality", "quality", quality)
		add("select", "image_size", "image_size", size)

	case ModelSeedream50Lite:
		width, height, err := seedreamLiteDimensions(effectiveSize, request.AspectRatio)
		if err != nil {
			return payload, err
		}
		add("input_boolean", "force_single", "force_single", "false")
		add("input_number", "seed", "seed", strconv.FormatInt(seed, 10))
		add("input_number", "min_ratio", "min_ratio", "0.07")
		add("input_number", "max_ratio", "max_ratio", "16")
		add("select", "cot_mode", "cot_mode", "enable")
		add("input_boolean", "close_search", "close_search", "false")
		add("input_boolean", "use_pre_llm", "use_pre_llm", "true")
		add("input_boolean", "allow_llm_fallback", "allow_llm_fallback", "true")
		add("input_number", "width", "width", strconv.Itoa(width))
		add("input_number", "height", "height", strconv.Itoa(height))
		add("text_area", "model_version", "model_version", "general_v5.0_M")
		add("text_area", "pre_vlm_version", "pre_vlm_version", "seed_x2i_50m_pe_mix_modelapi")

	case ModelNanoBanana2, ModelNanoBananaPro:
		ratio := normalizeAspectRatio(request.AspectRatio)
		if ratio == "" {
			ratio = "16:9"
		}
		if !oneOf(ratio, "16:9", "9:16", "4:3", "3:4", "1:1") {
			return payload, fmt.Errorf("%w: unsupported Nano Banana aspect ratio %q", ErrInvalidParams, request.AspectRatio)
		}
		size, err := nanoImageTier(effectiveSize, request.Quality)
		if err != nil {
			return payload, err
		}
		add("select", "aspect_ratio", "aspect_ratio", ratio)
		add("select", "image_size", "image_size", size)

	default:
		return payload, fmt.Errorf("%w: unsupported model %q", ErrInvalidParams, spec.Key)
	}

	imageInternalName := "img"
	if spec.Key == ModelSeedream50Pro {
		imageInternalName = "image"
	}
	for _, uri := range refs {
		payload.Inputs = append(payload.Inputs, inferenceInput{
			Format: "image_upload", Name: "img", InternalName: imageInternalName,
			Label: "img", Type: "uri", Value: uri,
		})
	}
	return payload, nil
}

// RequiredCredits returns Lumina's upstream computing-point charge for one
// image. The values mirror BytePlus' public cost_center/measure_configs rules
// (verified 2026-08-31) and deliberately use the same normalization helpers as
// buildCreateTaskPayload, so scheduling is based on the canvas actually sent.
// Credits are priced in tenths because Seedream Lite costs 3.5 and Seedream Pro
// charges 0.3 for every reference image after the first.
func RequiredCredits(request ImageRequest) (float64, error) {
	spec, ok := LookupModel(request.Model)
	if !ok {
		return 0, fmt.Errorf("%w: unsupported model %q", ErrInvalidParams, request.Model)
	}
	effectiveSize := strings.TrimSpace(request.Resolution)
	if effectiveSize == "" {
		effectiveSize = request.Size
	}

	switch spec.Key {
	case ModelSeedream50Pro:
		size, err := seedreamProSize(effectiveSize, request.AspectRatio)
		if err != nil {
			return 0, err
		}
		width, height, ok := parseDimensions(size)
		if !ok {
			return 0, fmt.Errorf("%w: invalid normalized Seedream Pro size %q", ErrInvalidParams, size)
		}
		credits := 9.0
		if width*height > 2360000 {
			credits = 18
		}
		if extraReferences := len(request.References) - 1; extraReferences > 0 {
			credits += float64(extraReferences) * 0.3
		}
		return credits, nil

	case ModelGPTImage2:
		quality := strings.ToLower(strings.TrimSpace(request.Quality))
		if quality == "" {
			quality = "low"
		}
		if !oneOf(quality, "low", "medium", "high") {
			return 0, fmt.Errorf("%w: unsupported GPT Image 2 quality %q", ErrInvalidParams, request.Quality)
		}
		size, err := gptImageSize(effectiveSize, request.AspectRatio)
		if err != nil {
			return 0, err
		}
		sizeClass := 0
		switch size {
		case "2048x1152", "2048x2048":
			sizeClass = 1
		case "3840x2160", "2160x3840":
			sizeClass = 2
		}
		prices := map[string][3]float64{
			"low":    {1, 3, 7},
			"medium": {8, 25, 60},
			"high":   {30, 96, 230},
		}
		return prices[quality][sizeClass], nil

	case ModelSeedream50Lite:
		if _, _, err := seedreamLiteDimensions(effectiveSize, request.AspectRatio); err != nil {
			return 0, err
		}
		return 3.5, nil

	case ModelNanoBanana2, ModelNanoBananaPro:
		tier, err := nanoImageTier(effectiveSize, request.Quality)
		if err != nil {
			return 0, err
		}
		if tier == "4K" {
			return 24, nil
		}
		return 12, nil
	default:
		return 0, fmt.Errorf("%w: unsupported model %q", ErrInvalidParams, request.Model)
	}
}

func seedreamProSize(size, aspectRatio string) (string, error) {
	if width, height, ok := parseDimensions(size); ok {
		if width < 1024 || height < 1024 || width > 2048 || height > 2048 {
			return "", fmt.Errorf("%w: Seedream Pro dimensions must be 1024..2048", ErrInvalidParams)
		}
		return fmt.Sprintf("%dx%d", width, height), nil
	}
	tier := strings.ToLower(strings.TrimSpace(size))
	if tier != "" && tier != "1k" && tier != "2k" {
		return "", fmt.Errorf("%w: unsupported Seedream Pro size %q", ErrInvalidParams, size)
	}
	longSide := 1024
	if tier == "2k" {
		longSide = 2048
	}
	return dimensionsForAspect(aspectRatio, longSide, 1024)
}

func seedreamLiteDimensions(size, aspectRatio string) (int, int, error) {
	if width, height, ok := parseDimensions(size); ok {
		if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
			return 0, 0, fmt.Errorf("%w: invalid Seedream Lite dimensions", ErrInvalidParams)
		}
		return width, height, nil
	}
	tier := strings.ToLower(strings.TrimSpace(size))
	if tier != "" && tier != "1k" && tier != "2k" && tier != "4k" {
		return 0, 0, fmt.Errorf("%w: unsupported Seedream Lite size %q", ErrInvalidParams, size)
	}
	longSide := 2048
	if tier == "1k" {
		longSide = 1024
	} else if tier == "4k" {
		longSide = 4096
	}
	dimensions, err := dimensionsForAspect(aspectRatio, longSide, 1)
	if err != nil {
		return 0, 0, err
	}
	width, height, _ := parseDimensions(dimensions)
	return width, height, nil
}

// gptImageSize adapts the application's 1K/2K/4K catalog tiers to Lumina's
// discrete image_size select. Lumina exposes image_size independently from its
// low/medium/high quality select, so quality must not silently promote the
// canvas (for example, a 1K portrait request must never become 2160x3840 just
// because the closest exact 9:16 choice is a 4K canvas).
func gptImageSize(size, aspectRatio string) (string, error) {
	allowed := []string{"1024x1024", "1536x1024", "1024x1536", "2048x2048", "2048x1152", "3840x2160", "2160x3840"}
	normalized := strings.ToLower(strings.TrimSpace(size))
	for _, value := range allowed {
		if normalized == value {
			return value, nil
		}
	}
	if normalized != "" && !oneOf(strings.ToUpper(normalized), "1K", "2K", "4K") {
		return "", fmt.Errorf("%w: unsupported GPT Image 2 size %q", ErrInvalidParams, size)
	}
	tier := strings.ToUpper(normalized)
	if tier == "" {
		tier = "1K"
	}
	ratio := normalizeAspectRatio(aspectRatio)
	if ratio == "" {
		ratio = "1:1"
	}
	if !oneOf(ratio, "1:1", "16:9", "9:16", "4:3", "3:4") {
		return "", fmt.Errorf("%w: unsupported GPT Image 2 aspect ratio %q", ErrInvalidParams, aspectRatio)
	}

	// The upstream menu has no complete tier x ratio Cartesian product. Pick a
	// stable, documented compatibility value per tier, preferring the requested
	// orientation and avoiding promotion to a larger tier. At 4K the only large
	// canvases are 16:9 and 9:16, so 4:3/3:4 use the matching orientation.
	switch tier {
	case "1K":
		switch ratio {
		case "1:1":
			return "1024x1024", nil
		case "16:9", "4:3":
			return "1536x1024", nil
		default:
			return "1024x1536", nil
		}
	case "2K":
		switch ratio {
		case "1:1":
			return "2048x2048", nil
		case "16:9":
			return "2048x1152", nil
		case "4:3":
			return "1536x1024", nil
		default:
			return "1024x1536", nil
		}
	case "4K":
		switch ratio {
		case "1:1":
			return "2048x2048", nil
		case "16:9", "4:3":
			return "3840x2160", nil
		default:
			return "2160x3840", nil
		}
	default:
		return "", fmt.Errorf("%w: unsupported GPT Image 2 size %q", ErrInvalidParams, size)
	}
}

func nanoImageTier(size, quality string) (string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(size))
	if oneOf(normalized, "1K", "2K", "4K") {
		return normalized, nil
	}
	if width, height, ok := parseDimensions(size); ok {
		longSide := width
		if height > longSide {
			longSide = height
		}
		switch {
		case longSide <= 1536:
			return "1K", nil
		case longSide <= 2560:
			return "2K", nil
		default:
			return "4K", nil
		}
	}
	if normalized != "" {
		return "", fmt.Errorf("%w: unsupported Nano Banana size %q", ErrInvalidParams, size)
	}
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "high":
		return "4K", nil
	case "medium":
		return "2K", nil
	default:
		return "1K", nil
	}
}

func dimensionsForAspect(aspectRatio string, longSide, minSide int) (string, error) {
	ratio := normalizeAspectRatio(aspectRatio)
	if ratio == "" {
		return fmt.Sprintf("%dx%d", longSide, longSide), nil
	}
	parts := strings.Split(ratio, ":")
	if len(parts) != 2 {
		return "", fmt.Errorf("%w: invalid aspect ratio %q", ErrInvalidParams, aspectRatio)
	}
	w, _ := strconv.Atoi(parts[0])
	h, _ := strconv.Atoi(parts[1])
	if w <= 0 || h <= 0 {
		return "", fmt.Errorf("%w: invalid aspect ratio %q", ErrInvalidParams, aspectRatio)
	}
	width, height := longSide, longSide
	if w >= h {
		height = int(math.Round(float64(longSide) * float64(h) / float64(w)))
		if height < minSide {
			height = minSide
			width = int(math.Round(float64(height) * float64(w) / float64(h)))
		}
	} else {
		width = int(math.Round(float64(longSide) * float64(w) / float64(h)))
		if width < minSide {
			width = minSide
			height = int(math.Round(float64(width) * float64(h) / float64(w)))
		}
	}
	if width > 2048 && minSide == 1024 {
		width = 2048
	}
	if height > 2048 && minSide == 1024 {
		height = 2048
	}
	return fmt.Sprintf("%dx%d", width, height), nil
}

func parseDimensions(value string) (int, int, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	var width, height int
	if _, err := fmt.Sscanf(value, "%dx%d", &width, &height); err != nil || width <= 0 || height <= 0 {
		return 0, 0, false
	}
	if fmt.Sprintf("%dx%d", width, height) != value {
		return 0, 0, false
	}
	return width, height, true
}

func normalizeAspectRatio(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "x", ":")
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func extractTaskID(created any) string {
	root, ok := created.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"parent_task_id", "task_id", "id"} {
		if value := strings.TrimSpace(stringValue(root[key])); value != "" {
			return value
		}
	}
	if task, ok := root["task"].(map[string]any); ok {
		return extractTaskID(task)
	}
	return ""
}

func (c *Client) pollImageTask(ctx context.Context, cookie, taskID string) (map[string]any, string, string, string, error) {
	query := map[string]any{
		"page_num": 1, "page_size": 1, "source": []any{}, "ids": []string{taskID},
	}
	for {
		data, err := c.apiData(ctx, http.MethodPost, "/inference/task/query_task_list", cookie, query)
		if err != nil {
			if contextErr := imageTaskContextError(ctx); contextErr != nil {
				return nil, "", "", imageTaskContextStatus(ctx), contextErr
			}
			if !errors.Is(err, ErrTemporaryUpstream) {
				return nil, "", "", "", err
			}
			// A transient query failure says nothing about the accepted task. Retry
			// this same parent id until the shared post-acceptance deadline expires.
			if waitErr := waitImageRetry(ctx); waitErr != nil {
				return nil, "", "", imageTaskContextStatus(ctx), waitErr
			}
			continue
		}
		root, _ := data.(map[string]any)
		tasks, _ := root["tasks"].([]any)
		if len(tasks) > 0 {
			task, _ := tasks[0].(map[string]any)
			status := strings.ToLower(strings.TrimSpace(stringValue(task["status"])))
			if imageURL, contentType := taskImageOutput(task); imageURL != "" && (status == "complete" || status == "partial_success" || status == "") {
				if status == "" {
					status = "complete"
				}
				return task, imageURL, contentType, status, nil
			}
			switch status {
			case "", "queue", "running":
				// The parent can be present before a child has been attached.
			case "complete", "partial_success":
				if childStatus := terminalChildStatus(task); childStatus != "" {
					return nil, "", "", status, taskStatusError(childStatus, taskFailureText(task))
				}
				return nil, "", "", status, fmt.Errorf("%w: completed task has no image output", ErrTemporaryUpstream)
			default:
				if retryErr := retryableBetaTaskError(task, status); retryErr != nil {
					return nil, "", "", status, retryErr
				}
				return nil, "", "", status, taskStatusError(status, taskFailureText(task))
			}
		}

		if waitErr := waitImageRetry(ctx); waitErr != nil {
			return nil, "", "", imageTaskContextStatus(ctx), waitErr
		}
	}
}

func waitImageRetry(ctx context.Context) error {
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return imageTaskContextError(ctx)
	case <-timer.C:
		return nil
	}
}

func imageTaskContextStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "cancel"
}

func imageTaskContextError(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: image task timed out", ErrTemporaryUpstream)
	}
	return ctx.Err()
}

// openAcceptedAsset retries only the URL produced by the already accepted
// task. It is intentionally bounded; exhausting it surfaces ErrTaskAccepted to
// the service instead of ever returning to create_task.
func (c *Client) openAcceptedAsset(ctx context.Context, cookie, imageURL string) ([]byte, string, error) {
	var lastErr error
	for attempt := 0; attempt < maxAssetDownloadAttempts; attempt++ {
		asset, contentType, err := c.OpenAsset(ctx, cookie, imageURL)
		if err == nil {
			return asset, contentType, nil
		}
		lastErr = err
		if !errors.Is(err, ErrTemporaryUpstream) || attempt+1 >= maxAssetDownloadAttempts {
			return nil, "", err
		}
		if waitErr := waitImageRetry(ctx); waitErr != nil {
			return nil, "", waitErr
		}
	}
	return nil, "", lastErr
}

func taskImageOutput(task map[string]any) (string, string) {
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		if output, contentType := outputsImageURL(child["multi_outputs"], task); output != "" {
			return output, contentType
		}
		if output, contentType := outputsImageURL(child["output"], task); output != "" {
			return output, contentType
		}
	}
	if output, contentType := outputsImageURL(task["multi_outputs"], task); output != "" {
		return output, contentType
	}
	return outputsImageURL(task["output"], task)
}

func outputsImageURL(raw any, task map[string]any) (string, string) {
	switch value := raw.(type) {
	case []any:
		for _, item := range value {
			if output, contentType := outputsImageURL(item, task); output != "" {
				return output, contentType
			}
		}
	case map[string]any:
		primary := strings.TrimSpace(stringValue(value["value"]))
		cover := strings.TrimSpace(stringValue(value["cover"]))
		candidate := resolveTaskResource(task, primary)
		if !isAssetReference(candidate) {
			candidate = resolveTaskResource(task, cover)
		}
		if isAssetReference(candidate) {
			contentType := strings.TrimSpace(stringValue(value["type"]))
			if format := strings.TrimSpace(stringValue(value["format"])); strings.Contains(format, "/") {
				contentType = format
			} else if contentType == "" && format != "" {
				contentType = "image/" + strings.TrimPrefix(strings.ToLower(format), ".")
			}
			return candidate, contentType
		}
	case string:
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
			var decoded any
			decoder := json.NewDecoder(strings.NewReader(value))
			decoder.UseNumber()
			if decoder.Decode(&decoded) == nil {
				return outputsImageURL(decoded, task)
			}
		}
		value = resolveTaskResource(task, value)
		if isAssetReference(value) {
			return value, ""
		}
	}
	return "", ""
}

func resolveTaskResource(task map[string]any, value string) string {
	if value == "" || isAssetReference(value) {
		return value
	}
	for _, key := range []string{"resources", "resource_map"} {
		resources, _ := task[key].(map[string]any)
		if resolved := strings.TrimSpace(stringValue(resources[value])); resolved != "" {
			if record, ok := resources[value].(map[string]any); ok {
				for _, field := range []string{"url", "value", "cover"} {
					if candidate := strings.TrimSpace(stringValue(record[field])); candidate != "" {
						return candidate
					}
				}
			}
			return resolved
		}
	}
	return value
}

func isAssetReference(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "/")
}

func terminalChildStatus(task map[string]any) string {
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		status := strings.ToLower(strings.TrimSpace(stringValue(child["status"])))
		if status != "" && status != "complete" && status != "partial_success" && status != "queue" && status != "running" {
			return status
		}
	}
	return ""
}

func taskFailureText(task map[string]any) string {
	for _, key := range []string{"fail_reason", "message", "error"} {
		if value := strings.TrimSpace(stringValue(task[key])); value != "" && value != "null" {
			return value
		}
	}
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		for _, key := range []string{"fail_reason", "message", "error"} {
			if value := strings.TrimSpace(stringValue(child[key])); value != "" && value != "null" {
				return value
			}
		}
	}
	return "task failed"
}

const betaInstabilityFailureReason = "The model is in beta and may be unstable occasionally, please try again later"

// retryableBetaTaskError recognizes only the exact GPT Image 2 terminal shape
// observed in production. Status and reason must come from the same child;
// combining fields from different children or another beta model could turn an
// unknown paid failure into a resubmit.
func retryableBetaTaskError(task map[string]any, parentStatus string) error {
	if strings.ToLower(strings.TrimSpace(parentStatus)) != "failed" {
		return nil
	}
	gptImage, ok := LookupModel("gpt-image-2")
	if !ok || taskInferenceID(task) != gptImage.ID {
		return nil
	}
	children, _ := task["children"].([]any)
	for _, rawChild := range children {
		child, _ := rawChild.(map[string]any)
		status := strings.ToLower(strings.TrimSpace(stringValue(child["status"])))
		reason := strings.TrimSpace(stringValue(child["fail_reason"]))
		if status == "downstream_execute" && reason == betaInstabilityFailureReason {
			return fmt.Errorf("%w: task %s: %s", ErrRetryableTaskFailed, status, clipString(reason, 240))
		}
	}
	return nil
}

func taskStatusError(status, reason string) error {
	status = strings.ToLower(strings.TrimSpace(status))
	base := ErrTaskFailed
	switch status {
	case "limit":
		base = ErrQuotaExhausted
	case "risk":
		base = ErrRiskControl
	case "invalid_param", "no_face_detected", "multi_face_detected":
		base = ErrInvalidParams
	}
	if classified := classifyUpstreamError(200, 0, reason); !errors.Is(classified, ErrTemporaryUpstream) {
		base = errorClass(classified)
	}
	return fmt.Errorf("%w: task %s: %s", base, status, clipString(reason, 240))
}

func errorClass(err error) error {
	for _, candidate := range []error{ErrAuth, ErrQuotaExhausted, ErrRiskControl, ErrInvalidParams} {
		if errors.Is(err, candidate) {
			return candidate
		}
	}
	return ErrTemporaryUpstream
}

// OpenAsset downloads an auth-gated Lumina result. The URL is restricted to
// BytePlus-owned asset hosts (or the configured API host in tests) before the
// account Cookie header is attached.
func (c *Client) OpenAsset(ctx context.Context, cookie, rawURL string) ([]byte, string, error) {
	cookie = normalizeCookie(cookie)
	if cookie == "" {
		return nil, "", ErrAuth
	}
	assetURL, err := normalizeAssetURL(rawURL)
	if err != nil {
		return nil, "", err
	}
	client, err := c.newHTTPClient(5*time.Minute, cookie)
	if err != nil {
		return nil, "", err
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many asset redirects")
		}
		if !allowedAssetURL(req.URL) {
			return errors.New("asset redirect left BytePlus domains")
		}
		setAssetHeaders(req, cookie)
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: invalid asset URL", ErrInvalidParams)
	}
	setAssetHeaders(req, cookie)
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: asset download: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := readLimited(resp.Body, 8<<10)
		classified := classifyUpstreamError(resp.StatusCode, 0, responseMessage(raw))
		if !sameAPIOrigin(resp.Request.URL) && errors.Is(classified, ErrAuth) {
			// Public CDN requests never carry the Lumina Cookie. Their 401/403 can
			// only describe the asset URL, so it is not evidence that the durable
			// account session is invalid.
			classified = fmt.Errorf("%w: BytePlus asset authorization rejected", ErrTemporaryUpstream)
		}
		return nil, "", classified
	}
	if resp.ContentLength > maxAssetBytes {
		return nil, "", fmt.Errorf("%w: asset download exceeds size limit", ErrTemporaryUpstream)
	}
	raw, err := readLimited(resp.Body, maxAssetBytes)
	if err != nil {
		return nil, "", fmt.Errorf("%w: asset download: %v", ErrTemporaryUpstream, err)
	}
	detectedType := netguard.DetectMediaType(raw)
	if !strings.HasPrefix(detectedType, "image/") || detectedType == "image/svg+xml" {
		return nil, "", fmt.Errorf("%w: asset response is not an image", ErrTemporaryUpstream)
	}
	// Payload magic is authoritative. BytePlus resource-utils and CDN responses
	// can carry stale image/png headers for JPEG/WebP/AVIF results.
	return raw, detectedType, nil
}

func setAssetHeaders(req *http.Request, cookie string) {
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,image/*,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Referer", webReferer)
	req.Header.Set("User-Agent", userAgent)
	// Only resource-utils URLs on the Lumina API origin need the authenticated
	// browser session. Never forward that durable credential to a CDN, even a
	// BytePlus-owned one, or across an allowed cross-origin redirect.
	if sameAPIOrigin(req.URL) {
		req.Header.Set("Cookie", cookie)
	} else {
		req.Header.Del("Cookie")
		req.Header.Del("X-Csrf-Token")
	}
}

func normalizeAssetURL(rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if strings.HasPrefix(rawURL, "//") {
		return "", fmt.Errorf("%w: untrusted BytePlus asset URL", ErrInvalidParams)
	}
	if strings.HasPrefix(rawURL, "/") {
		base, err := url.Parse(apiBaseURL)
		if err != nil || base.Scheme == "" || base.Host == "" {
			return "", fmt.Errorf("%w: invalid BytePlus API base URL", ErrTemporaryUpstream)
		}
		apiPath := strings.TrimRight(base.EscapedPath(), "/")
		if apiPath != "" && (rawURL == apiPath || strings.HasPrefix(rawURL, apiPath+"/")) {
			rawURL = base.Scheme + "://" + base.Host + rawURL
		} else {
			rawURL = strings.TrimRight(apiBaseURL, "/") + rawURL
		}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.User != nil || parsed.Host == "" || !allowedAssetURL(parsed) {
		return "", fmt.Errorf("%w: untrusted BytePlus asset URL", ErrInvalidParams)
	}
	return parsed.String(), nil
}

func sameAPIOrigin(parsed *url.URL) bool {
	if parsed == nil || parsed.User != nil {
		return false
	}
	apiURL, err := url.Parse(apiBaseURL)
	return err == nil && strings.EqualFold(parsed.Scheme, apiURL.Scheme) && strings.EqualFold(parsed.Host, apiURL.Host)
}

func allowedAssetURL(parsed *url.URL) bool {
	if parsed == nil || parsed.Hostname() == "" {
		return false
	}
	if sameAPIOrigin(parsed) {
		return true
	}
	if parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, suffix := range []string{".bytepluses.com", ".bytepluscdn.com", ".byteoversea.com", ".bytevcloudapi.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return host == "bytepluses.com" || host == "bytepluscdn.com" || host == "byteoversea.com"
}

type imageXToken struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	ExpiredTime     string
}

func (c *Client) fetchImageXToken(ctx context.Context, cookie string) (*imageXToken, error) {
	data, err := c.apiData(ctx, http.MethodGet, "/egress_gateway/imagex_upload_token", cookie, nil)
	if err != nil {
		return nil, err
	}
	values, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: upload token response is malformed", ErrTemporaryUpstream)
	}
	token := &imageXToken{
		AccessKeyID:     firstMapString(values, "AccessKeyId", "AccessKeyID", "access_key_id", "accessKeyId"),
		SecretAccessKey: firstMapString(values, "SecretAccessKey", "secret_access_key", "secretAccessKey"),
		SessionToken:    firstMapString(values, "SessionToken", "session_token", "sessionToken"),
		ExpiredTime:     firstMapString(values, "ExpiredTime", "expired_time", "expiredTime"),
	}
	if token.AccessKeyID == "" || token.SecretAccessKey == "" || token.SessionToken == "" {
		return nil, fmt.Errorf("%w: upload token is incomplete", ErrTemporaryUpstream)
	}
	return token, nil
}

func firstMapString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(values[key])); value != "" {
			return value
		}
	}
	return ""
}

type imageXApplyResponse struct {
	ResponseMetadata imageXResponseMetadata `json:"ResponseMetadata"`
	Result           struct {
		UploadAddress struct {
			StoreInfos []struct {
				StoreURI string `json:"StoreUri"`
				Auth     string `json:"Auth"`
			} `json:"StoreInfos"`
			UploadHosts  []string          `json:"UploadHosts"`
			SessionKey   string            `json:"SessionKey"`
			UploadHeader map[string]string `json:"UploadHeader"`
		} `json:"UploadAddress"`
	} `json:"Result"`
}

type imageXResponseMetadata struct {
	RequestID string `json:"RequestId"`
	Error     *struct {
		Code    string `json:"Code"`
		CodeN   int    `json:"CodeN"`
		Message string `json:"Message"`
	} `json:"Error"`
}

func (c *Client) uploadImageX(ctx context.Context, cookie string, token imageXToken, userID, userName string, index int, raw []byte) (string, error) {
	filename := fmt.Sprintf("ref_%d_%d%s", time.Now().UnixMilli(), index+1, imageExtension(raw))
	storeKey := normalizeStoreKey(fmt.Sprintf("%s-%d-%s", userID, time.Now().UnixMilli(), filename))
	query := url.Values{
		"Action":          {"ApplyImageUpload"},
		"Version":         {"2018-08-01"},
		"ServiceId":       {imageXServiceID},
		"FileSize":        {strconv.Itoa(len(raw))},
		"StoreKeys":       {storeKey},
		"s":               {randomBase36(12)},
		"device_platform": {"web"},
	}
	applyURL := strings.TrimRight(imageXEndpointURL, "/") + "/?" + encodeAWSQuery(query)
	applyReq, err := http.NewRequestWithContext(ctx, http.MethodGet, applyURL, nil)
	if err != nil {
		return "", fmt.Errorf("%w: create ImageX apply request", ErrTemporaryUpstream)
	}
	signImageXRequest(applyReq, token, nil, time.Now().UTC())
	applyRaw, err := c.doImageX(ctx, cookie, applyReq, maxAPIResponseBytes)
	if err != nil {
		return "", err
	}
	var applied imageXApplyResponse
	if json.Unmarshal(applyRaw, &applied) != nil {
		return "", fmt.Errorf("%w: ImageX apply returned non-json", ErrTemporaryUpstream)
	}
	if applied.ResponseMetadata.Error != nil {
		upstream := applied.ResponseMetadata.Error
		return "", classifyUpstreamError(200, upstream.CodeN, upstream.Message+" "+upstream.Code)
	}
	address := applied.Result.UploadAddress
	if len(address.StoreInfos) == 0 || len(address.UploadHosts) == 0 || address.SessionKey == "" {
		return "", fmt.Errorf("%w: ImageX apply response missing upload address", ErrTemporaryUpstream)
	}
	store := address.StoreInfos[0]
	if store.StoreURI == "" || store.Auth == "" {
		return "", fmt.Errorf("%w: ImageX apply response missing store info", ErrTemporaryUpstream)
	}

	uploadURL, err := imageXUploadURL(address.UploadHosts[0], store.StoreURI)
	if err != nil {
		return "", err
	}
	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: create ImageX upload request", ErrTemporaryUpstream)
	}
	uploadReq.Header.Set("Authorization", store.Auth)
	uploadReq.Header.Set("Content-CRC32", fmt.Sprintf("%08x", crc32.ChecksumIEEE(raw)))
	uploadReq.Header.Set("X-Storage-U", encodeURIComponent("ByteArtist_User_"+userName))
	uploadReq.Header.Set("Content-Type", "application/octet-stream")
	uploadReq.Header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	for key, value := range address.UploadHeader {
		uploadReq.Header.Set(key, value)
	}
	uploadRaw, err := c.doImageX(ctx, cookie, uploadReq, maxAPIResponseBytes)
	if err != nil {
		return "", err
	}
	var uploaded map[string]any
	decoder := json.NewDecoder(bytes.NewReader(uploadRaw))
	decoder.UseNumber()
	if decoder.Decode(&uploaded) != nil {
		return "", fmt.Errorf("%w: ImageX upload returned non-json", ErrTemporaryUpstream)
	}
	if code, ok := intValue(uploaded["code"]); !ok || code != 2000 {
		return "", classifyUpstreamError(200, code, strings.TrimSpace(stringValue(uploaded["message"])))
	}

	commitBody, _ := json.Marshal(map[string]any{"SessionKey": address.SessionKey})
	commitQuery := url.Values{
		"Action":    {"CommitImageUpload"},
		"Version":   {"2018-08-01"},
		"ServiceId": {imageXServiceID},
	}
	commitURL := strings.TrimRight(imageXEndpointURL, "/") + "/?" + encodeAWSQuery(commitQuery)
	commitReq, err := http.NewRequestWithContext(ctx, http.MethodPost, commitURL, bytes.NewReader(commitBody))
	if err != nil {
		return "", fmt.Errorf("%w: create ImageX commit request", ErrTemporaryUpstream)
	}
	commitReq.Header.Set("Content-Type", "application/json")
	signImageXRequest(commitReq, token, commitBody, time.Now().UTC())
	commitRaw, err := c.doImageX(ctx, cookie, commitReq, maxAPIResponseBytes)
	if err != nil {
		return "", err
	}
	var committed struct {
		ResponseMetadata imageXResponseMetadata `json:"ResponseMetadata"`
	}
	if json.Unmarshal(commitRaw, &committed) != nil {
		return "", fmt.Errorf("%w: ImageX commit returned non-json", ErrTemporaryUpstream)
	}
	if committed.ResponseMetadata.Error != nil {
		upstream := committed.ResponseMetadata.Error
		return "", classifyUpstreamError(200, upstream.CodeN, upstream.Message+" "+upstream.Code)
	}

	riskData, err := c.apiData(ctx, http.MethodPost, "/egress_gateway/risk_predict", cookie, map[string]any{
		"risk_check_type": 1, "imagex_uris": []string{store.StoreURI},
	})
	if err != nil {
		return "", err
	}
	if err := validateRiskResult(riskData); err != nil {
		return "", err
	}
	return store.StoreURI, nil
}

func validateRiskResult(data any) error {
	risk, ok := data.(map[string]any)
	if !ok {
		return fmt.Errorf("%w: risk response is malformed", ErrTemporaryUpstream)
	}
	hit, exists := risk["hit"]
	flag, valid := hit.(bool)
	if !exists || !valid {
		return fmt.Errorf("%w: risk response is missing or has invalid hit flag", ErrTemporaryUpstream)
	}
	if flag {
		return fmt.Errorf("%w: reference image was rejected", ErrRiskControl)
	}
	return nil
}

func (c *Client) doImageX(ctx context.Context, cookie string, req *http.Request, limit int64) ([]byte, error) {
	client, err := c.newHTTPClient(5*time.Minute, cookie)
	if err != nil {
		return nil, err
	}
	// Apply/Commit signatures and upload Store Auth are scoped to the original
	// ImageX endpoint. Treat redirects as upstream failures instead of letting
	// net/http forward either credential to a response-selected destination.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w: ImageX request: %v", ErrTemporaryUpstream, err)
	}
	defer resp.Body.Close()
	raw, readErr := readLimited(resp.Body, limit)
	if readErr != nil {
		return nil, fmt.Errorf("%w: ImageX response: %v", ErrTemporaryUpstream, readErr)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// These endpoints authenticate with the short-lived ImageX token or Store
		// Auth returned for this upload, not the durable Lumina browser Cookie. A
		// rejected signature/token therefore cannot prove that the account session
		// is dead and must not cause the pool scheduler to disable it.
		return nil, fmt.Errorf("%w: ImageX upload authorization rejected", ErrTemporaryUpstream)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, classifyUpstreamError(resp.StatusCode, 0, responseMessage(raw))
	}
	return raw, nil
}

func signImageXRequest(req *http.Request, token imageXToken, body []byte, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	dateStamp := now.UTC().Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Security-Token", token.SessionToken)
	if body != nil {
		req.Header.Set("X-Amz-Content-Sha256", sha256Hex(body))
	}

	canonicalHeaders, signedHeaders := canonicalImageXHeaders(req)
	payloadHash := sha256Hex(body)
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		encodeAWSQuery(req.URL.Query()),
		canonicalHeaders + "\n",
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{dateStamp, imageXRegion, imageXService, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")
	signingKey := imageXSigningKey(token.SecretAccessKey, dateStamp)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		token.AccessKeyID, scope, signedHeaders, signature,
	))
}

func canonicalImageXHeaders(req *http.Request) (string, string) {
	excluded := map[string]bool{
		"authorization": true, "content-type": true, "content-length": true,
		"user-agent": true, "presigned-expires": true, "expect": true,
		"x-amzn-trace-id": true,
	}
	values := map[string]string{}
	for key, list := range req.Header {
		lower := strings.ToLower(key)
		if excluded[lower] && !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		values[lower] = strings.Join(strings.Fields(strings.Join(list, ",")), " ")
	}
	host := strings.TrimSpace(req.Host)
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	values["host"] = strings.ToLower(host)
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, key := range keys {
		canonical.WriteString(key)
		canonical.WriteByte(':')
		canonical.WriteString(values[key])
		canonical.WriteByte('\n')
	}
	return strings.TrimSuffix(canonical.String(), "\n"), strings.Join(keys, ";")
}

func imageXSigningKey(secret, dateStamp string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(imageXRegion))
	kService := hmacSHA256(kRegion, []byte(imageXService))
	return hmacSHA256(kService, []byte("aws4_request"))
}

func hmacSHA256(key, message []byte) []byte {
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write(message)
	return digest.Sum(nil)
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func encodeAWSQuery(values url.Values) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		encodedValues := append([]string(nil), values[key]...)
		sort.Strings(encodedValues)
		for _, value := range encodedValues {
			parts = append(parts, awsURIEncode(key, false)+"="+awsURIEncode(value, false))
		}
	}
	return strings.Join(parts, "&")
}

func awsURIEncode(value string, keepSlash bool) string {
	var encoded strings.Builder
	for _, char := range []byte(value) {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' || char == '~' || (char == '/' && keepSlash) {
			encoded.WriteByte(char)
		} else {
			fmt.Fprintf(&encoded, "%%%02X", char)
		}
	}
	return encoded.String()
}

func encodeURIComponent(value string) string {
	encoded := url.QueryEscape(value)
	encoded = strings.ReplaceAll(encoded, "+", "%20")
	for escaped, literal := range map[string]string{
		"%21": "!", "%27": "'", "%28": "(", "%29": ")", "%2A": "*",
	} {
		encoded = strings.ReplaceAll(encoded, escaped, literal)
	}
	return encoded
}

func imageXUploadURL(host, storeURI string) (string, error) {
	host = strings.TrimSpace(host)
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	parsed, err := url.Parse(host)
	if err != nil || parsed.User != nil || parsed.Host == "" || !allowedImageXUploadURL(parsed) {
		return "", fmt.Errorf("%w: invalid ImageX upload host", ErrTemporaryUpstream)
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSpace(storeURI) == "" {
		return "", fmt.Errorf("%w: invalid ImageX upload address", ErrTemporaryUpstream)
	}
	// StoreUri is already an ImageX URI, not a local filesystem path. Preserve
	// it byte-for-byte as the web SDK does instead of cleaning it with path.Join.
	base := parsed.Scheme + "://" + parsed.Host + strings.TrimRight(parsed.EscapedPath(), "/")
	return base + "/upload/v1/" + strings.TrimLeft(storeURI, "/"), nil
}

func allowedImageXUploadURL(parsed *url.URL) bool {
	if parsed == nil || parsed.Hostname() == "" || parsed.User != nil {
		return false
	}
	endpoint, _ := url.Parse(imageXEndpointURL)
	if endpoint != nil && strings.EqualFold(parsed.Scheme, endpoint.Scheme) && strings.EqualFold(parsed.Host, endpoint.Host) {
		return true
	}
	if parsed.Scheme != "https" || (parsed.Port() != "" && parsed.Port() != "443") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, root := range []string{"bytepluses.com", "bytepluscdn.com", "bytevcloudapi.com"} {
		if host == root || strings.HasSuffix(host, "."+root) {
			return true
		}
	}
	return false
}

func imageExtension(raw []byte) string {
	if extension, ok := netguard.MediaExtension(netguard.DetectMediaType(raw)); ok {
		return extension
	}
	return ".png"
}

func normalizeStoreKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("#", "_", "%", "_", "+", "_", ",", "_", "，", "_")
	return replacer.Replace(value)
}

func randomBase36(length int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if length <= 0 {
		return ""
	}
	raw := make([]byte, length)
	if _, err := crand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	for index := range raw {
		raw[index] = alphabet[int(raw[index])%len(alphabet)]
	}
	return string(raw)
}
