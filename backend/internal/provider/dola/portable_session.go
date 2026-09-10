package dola

import (
	"context"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"sync"
	"time"
)

// Access, including idle eviction, is serialized by browserLock. The request
// only owns a child context; its completion must not kill the verified browser.
type portableBrowser struct {
	ctx      context.Context
	close    func()
	key      string
	lastUsed time.Time
	idle     *time.Timer
}

const portableBrowserIdle = 20 * time.Minute
const portableBrowserLimit = 4

func (c *Client) acquirePortableBrowser(ctx context.Context, account Account, path, proxy string) (context.Context, func(), bool, error) {
	if c.portableBrowsers == nil {
		c.portableBrowsers = make(map[string]*portableBrowser)
	}
	id := account.ID
	if id == "" {
		id = IdentityHash(CookieValue(account.Cookie, "sessionid"))
	}
	key := dolaProfileDir(account) + "|" + proxy
	entry := c.portableBrowsers[id]
	if entry != nil && (entry.key != key || entry.ctx.Err() != nil) {
		if entry.idle != nil {
			entry.idle.Stop()
		}
		entry.close()
		delete(c.portableBrowsers, id)
		entry = nil
	}
	fresh := entry == nil
	if fresh {
		if len(c.portableBrowsers) >= portableBrowserLimit {
			oldestID := ""
			var oldest *portableBrowser
			for candidateID, candidate := range c.portableBrowsers {
				if oldest == nil || candidate.lastUsed.Before(oldest.lastUsed) {
					oldestID, oldest = candidateID, candidate
				}
			}
			if oldest != nil {
				if oldest.idle != nil {
					oldest.idle.Stop()
				}
				oldest.close()
				delete(c.portableBrowsers, oldestID)
			}
		}
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		opts, bridge, err := dolaBrowserOptions(lifetime, path, proxy, dolaProfileDir(account))
		if err != nil {
			cancelLifetime()
			return nil, func() {}, false, err
		}
		alloc, cancelAlloc := chromedp.NewExecAllocator(lifetime, opts...)
		browser, cancelBrowser := chromedp.NewContext(alloc)
		closeBrowser := sync.OnceFunc(func() {
			cancelBrowser()
			cancelAlloc()
			cancelLifetime()
			if bridge != nil {
				bridge.Close()
			}
		})
		// Start on the durable context. Starting on a request child lets chromedp
		// bind the entire browser process to the request's deadline.
		stop := context.AfterFunc(ctx, cancelLifetime)
		err = chromedp.Run(browser, network.Enable())
		stop()
		if err != nil || ctx.Err() != nil {
			closeBrowser()
			if err == nil {
				err = ctx.Err()
			}
			return nil, func() {}, false, bootstrapFailure("chromium", err)
		}
		entry = &portableBrowser{ctx: browser, close: closeBrowser, key: key}
		c.portableBrowsers[id] = entry
	}
	if entry.idle != nil {
		entry.idle.Stop()
	}
	entry.lastUsed = time.Now()
	request, cancelRequest := context.WithTimeout(entry.ctx, videoSubmitWait)
	stop := context.AfterFunc(ctx, cancelRequest)
	release := sync.OnceFunc(func() {
		stop()
		cancelRequest()
		entry.lastUsed = time.Now()
		entry.idle = time.AfterFunc(portableBrowserIdle, func() {
			lock := c.browserLock()
			lock.Lock()
			defer lock.Unlock()
			if c.portableBrowsers[id] == entry && time.Since(entry.lastUsed) >= portableBrowserIdle {
				entry.close()
				delete(c.portableBrowsers, id)
			}
		})
	})
	return request, release, fresh, nil
}
