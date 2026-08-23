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
// the very same exit IP. The submit therefore always happens in the page, and
// only the render is followed from Go.
const (
	// The page is only needed until upstream accepts the job: the render itself is
	// then followed by polling the chat from Go, which keeps a browser out of the
	// minutes-long wait and lets many more submits share the host.
	inPageAcceptWait    = 3 * time.Minute
	inPageSubmitTimeout = inPageAcceptWait + time.Minute
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
// until upstream accepted the job, which carries the logId and the chat the
// caller then polls, plus any page-side failure that ended the read early.
type videoSubmitResult struct {
	Status  int    `json:"status"`
	ChatID  string `json:"chatId"`
	Stream  string `json:"stream"`
	Failure string `json:"failure"`
	Done    bool   `json:"done"`
	// Proxy is the session the submitting page egresses through, which the chat
	// polling then reuses so the whole generation is seen from one exit IP. The
	// page fills the rest of this struct, so it must stay out of the JSON.
	Proxy string `json:"-"`
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

// SubmitVideo mints a Banti token on a warm page of this account and runs both
// the create-chat and the stream request as fetches on that same page. The
// payload is the request the caller built; chatId, focusId, jt and bid are
// filled in by the page because only the page owns those values. It returns as
// soon as upstream accepts or rejects the job, so the page is handed back to the
// pool long before the clip is rendered.
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

// inPageSubmitScript runs the website's own request sequence: mint the Banti
// token, create the aiVideo chat, then read the event stream until upstream has
// either accepted the job or refused it. Failures are kept on the state object
// instead of rejecting so the collected stream survives.
func inPageSubmitScript(quotedPayload string) string {
	return `window.__oreateSubmit = {done: false, status: 0, chatId: "", stream: "", failure: ""};
		(async () => {
			const state = window.__oreateSubmit;
			try {
				const jt = await new Promise((resolve, reject) => {
					const timer = setTimeout(() => reject(new Error("banti timeout")), ` +
		strconv.Itoa(int(bantiResponseTimeout/time.Millisecond)) + `);
					window.paris_21a851acb0.getBantiInstance((instanceError, instance) => {
						if (instanceError || !instance) {
							clearTimeout(timer);
							reject(new Error("banti instance"));
							return;
						}
						if (instance.options) instance.options.reportTimeout = 20000;
						window.paris_21a851acb0.sendBantiReport({subid: ""}, (reportError, response) => {
							clearTimeout(timer);
							if (reportError) {
								reject(new Error("banti report"));
								return;
							}
							resolve(response && response.htj && response.htj.jt || "");
						});
					});
				});
				if (!jt) throw new Error("banti token empty");
				const chatResponse = await fetch("/oreate/create/chat", {
					method: "POST",
					credentials: "include",
					headers: {"content-type": "application/json", "client-type": "pc", "locale": "zh-CN"},
					body: JSON.stringify({type: "aiVideo"}),
				});
				const chat = await chatResponse.json();
				const chatId = chat && chat.data && chat.data.chatId || "";
				state.chatId = chatId;
				if (!chatId) throw new Error("create chat: " + JSON.stringify(chat).slice(0, 200));
				const payload = JSON.parse(` + quotedPayload + `);
				payload.jt = jt;
				payload.chatId = chatId;
				payload.focusId = chatId;
				payload.extra.bid = (document.cookie.match(/__bid_n=([^;]*)/) || [])[1] || payload.extra.bid || "";
				const response = await fetch("/oreate/sse/stream", {
					method: "POST",
					credentials: "include",
					headers: {
						"content-type": "application/json",
						"client-type": "pc",
						"locale": "zh-CN",
						"accept": "text/event-stream",
					},
					body: JSON.stringify(payload),
				});
				state.status = response.status;
				const reader = response.body.getReader();
				const decoder = new TextDecoder();
				const accepted = /"event"\s*:\s*"(start|generating)"/;
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
					if (state.stream.indexOf(".mp4") >= 0 || accepted.test(state.stream)) break;
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
		failure := strings.TrimSpace(result.Failure)
		if failure == "" {
			failure = "stream request did not start"
		}
		return 0, fmt.Errorf("%w: browser submit: %s", ErrTemporaryUpstream, failure)
	}
	if result.Status == http.StatusUnauthorized || result.Status == http.StatusForbidden {
		return result.Status, ErrAuth
	}
	return result.Status, nil
}
