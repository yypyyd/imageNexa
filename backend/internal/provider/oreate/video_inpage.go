package oreate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Risk control accepts the generation only when the request itself comes from a
// real signed page: an A/B run in production had every in-page submit accepted
// and 20 of 21 Go submits refused as spam, even with a freshly minted token and
// the very same exit IP. The signed page keeps the response open through the
// render because closing it after the start event can cancel the upstream job.
const (
	inPageRenderWait    = 32 * time.Minute
	inPageSubmitTimeout = inPageRenderWait + time.Minute
	inPageStreamLimit   = 8 << 20
	// The upstream stream pings while a clip renders, so a longer gap means the
	// connection died: stop the read and let logId recovery use what the page
	// already collected instead of idling until the poll timeout.
	inPageStreamStall = 90 * time.Second
)

// videoSubmitter submits a prepared generation request from inside the browser
// session that produced the Banti token, so token, TLS fingerprint, headers and
// cookie jar all come from one origin.
type videoSubmitter interface {
	SubmitVideo(ctx context.Context, account Account, payload []byte) (videoSubmitResult, error)
}

// videoSubmitResult is what the page observed for one submit: the SSE text read
// through the rendered result or a terminal failure, plus any page-side
// failure that ended the read early.
type videoSubmitResult struct {
	Status  int    `json:"status"`
	Stream  string `json:"stream"`
	Failure string `json:"failure"`
	Done    bool   `json:"done"`
	// Proxy is the session the submitting page egresses through.
	Proxy  string `json:"-"`
	Cookie string `json:"-"`
}

// availableCPUs reports the cores the process may use: runtime.NumCPU only sees
// the host, so a cgroup quota (the usual container limit) wins when present.
func availableCPUs() int {
	cpus := runtime.NumCPU()
	quota := readCgroupPair("/sys/fs/cgroup/cpu.max")
	if quota <= 0 {
		if q, p := readCgroupValue("/sys/fs/cgroup/cpu/cpu.cfs_quota_us"), readCgroupValue("/sys/fs/cgroup/cpu/cpu.cfs_period_us"); q > 0 && p > 0 {
			quota = (q + p - 1) / p
		}
	}
	if quota > 0 {
		cpus = min(cpus, int(quota))
	}
	return cpus
}

// availableMemory reports the memory ceiling in bytes, preferring the cgroup
// limit over the machine total, and 0 when neither can be read.
func availableMemory() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		// An unlimited cgroup reports "max" or a sentinel near the address space,
		// which says nothing about the machine and must not size the limit.
		if value := readCgroupValue(path); value > 0 && value < 1<<50 {
			return value
		}
	}
	return machineMemory()
}

// machineMemory reads the total RAM Linux reports; other platforms (developer
// machines) get 0 and are sized by CPU count alone.
func machineMemory() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb << 10
	}
	return 0
}

// readCgroupPair reads the "quota period" form used by cgroup v2 cpu.max and
// returns the whole cores it allows, rounded up.
func readCgroupPair(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0
	}
	quota, qErr := strconv.ParseInt(fields[0], 10, 64)
	period, pErr := strconv.ParseInt(fields[1], 10, 64)
	if qErr != nil || pErr != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return (quota + period - 1) / period
}

// readCgroupValue reads a cgroup file holding a single number, treating "max"
// and unparsable content as absent.
func readCgroupValue(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// SubmitVideo mints a Banti token on a warm page of this account and posts the
// stream request as a fetch on that same page. The payload is the request the
// caller built; jt and bid are filled in by the page because only the page owns
// those values. The page keeps reading the response until the clip or a
// terminal failure arrives.
func (s *chromiumSigner) SubmitVideo(ctx context.Context, account Account, payload []byte) (videoSubmitResult, error) {
	account = account.normalized()
	if account.Cookie == "" {
		return videoSubmitResult{}, ErrAuth
	}
	quoted, err := json.Marshal(string(payload))
	if err != nil {
		return videoSubmitResult{}, err
	}
	return s.pool.submit(ctx, account, string(quoted))
}

// inPageSubmitScript mints the Banti token and reads the event stream until
// the rendered clip or a terminal error arrives. Failures are kept on the
// state object instead of rejecting so the collected stream survives.
func inPageSubmitScript(quotedPayload string) string {
	return parisHelperJS + `window.__oreateSubmit = {done: false, status: 0, stream: "", failure: ""};
		(async () => {
			const state = window.__oreateSubmit;
			const mintJT = (subid) => new Promise((resolve, reject) => {
				const timer = setTimeout(() => reject(new Error("banti timeout")), ` +
		strconv.Itoa(int(bantiResponseTimeout/time.Millisecond)) + `);
				window.oreateSendBantiReport({subid: subid || ""}, (reportError, response, fallback) => {
					clearTimeout(timer);
					if (reportError) {
						reject(new Error("banti report"));
						return;
					}
					resolve((response && response.htj && response.htj.jt) || fallback || "");
				});
			});
			try {
				const minted = await Promise.all([mintJT("sse"), window.oreateGetAcsToken()]);
				const sseJT = minted[0];
				const acs = minted[1] || "600";
				if (!sseJT) throw new Error("banti token empty");
				const payload = JSON.parse(` + quotedPayload + `);
				payload.jt = sseJT;
				payload.ua = navigator.userAgent;
				payload.extra.bid = (document.cookie.match(/__bid_n=([^;]*)/) || [])[1] || payload.extra.bid || "";
				const response = await fetch("/oreate/sse/stream", {
					method: "POST",
					credentials: "include",
					headers: {
						"content-type": "application/json",
						"client-type": "pc",
						"locale": "zh-CN",
						"Acs-Token": acs,
						"JS-Token": sseJT,
						"accept": "text/event-stream",
					},
					body: JSON.stringify(payload),
				});
				state.status = response.status;
				const reader = response.body.getReader();
				const decoder = new TextDecoder();
				const terminalError = /"event"\s*:\s*"error"/;
				const stall = ` + strconv.Itoa(int(inPageStreamStall/time.Millisecond)) + `;
				while (state.stream.length < ` + strconv.Itoa(inPageStreamLimit) + `) {
					let stallTimer = 0;
					const chunk = await Promise.race([
						reader.read(),
						new Promise((_, reject) => {
							stallTimer = setTimeout(() => reject(new Error("stream stalled")), stall);
						}),
					]);
					clearTimeout(stallTimer);
					if (chunk.done) break;
					state.stream += decoder.decode(chunk.value, {stream: true});
					if (state.stream.indexOf(".mp4") >= 0 || terminalError.test(state.stream)) break;
				}
			} catch (error) {
				state.failure = String(error && error.message || error);
			}
			state.done = true;
		})(); true`
}

// inPageStreamStatus turns the page-side observations into the status and reader
// the SSE consumer expects, and reports the failures that never reached a stream.
func inPageStreamStatus(result videoSubmitResult) (int, error) {
	if result.Status == 0 {
		return 0, classifyInPageFailure(result.Failure)
	}
	if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
		return result.Status, ErrAuth
	}
	return result.Status, nil
}

func classifyInPageFailure(failure string) error {
	failure = strings.TrimSpace(failure)
	if failure == "" {
		failure = "stream request did not start"
	}
	if code := extractInPageStatusCode(failure); code != 0 {
		return classifyUpstreamError(code, failure)
	}
	lower := strings.ToLower(failure)
	if strings.Contains(failure, "请输入验证码") || strings.Contains(lower, "captcha") || strings.Contains(lower, "verification code") {
		return classifyUpstreamError(7350001, failure)
	}
	return fmt.Errorf("%w: browser submit: %s", ErrTemporaryUpstream, failure)
}

func extractInPageStatusCode(failure string) int {
	search := failure
	if i := strings.Index(failure, `"status"`); i >= 0 {
		search = failure[i:]
	}
	code := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(afterJSONKey(search, `"code"`), ":")), "%d", &code); err == nil {
		return code
	}
	return 0
}

func afterJSONKey(raw, key string) string {
	i := strings.Index(raw, key)
	if i < 0 {
		return ""
	}
	return raw[i+len(key):]
}
