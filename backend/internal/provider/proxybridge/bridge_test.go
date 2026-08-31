package proxybridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBridgeAuthenticatesUpstreamConnect(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "proxy-bridge-ok")
	}))
	defer target.Close()

	const proxyUser, proxyPass = "proxy-user", "proxy-pass"
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte(proxyUser+":"+proxyPass))
	var authenticated atomic.Bool
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != wantAuth {
			w.Header().Set("Proxy-Authenticate", `Basic realm="bridge-test"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		authenticated.Store(true)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.DialTimeout("tcp", r.Host, 5*time.Second)
		if err != nil {
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, client)
			if tcp, ok := upstream.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
			close(done)
		}()
		_, _ = io.Copy(client, upstream)
		<-done
	}))
	defer upstreamProxy.Close()

	rawProxy := "http://" + proxyUser + ":" + proxyPass + "@" + strings.TrimPrefix(upstreamProxy.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	bridge, err := Start(ctx, rawProxy)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer bridge.Close()
	bridgeURL, err := url.Parse(bridge.URL())
	if err != nil {
		t.Fatalf("bridge URL = %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyURL(bridgeURL),
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: 10 * time.Second,
	}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET through bridge = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response = %v", err)
	}
	if string(body) != "proxy-bridge-ok" || !authenticated.Load() {
		t.Fatalf("body/authenticated = %q/%t", body, authenticated.Load())
	}
}

func TestStartErrorDoesNotExposeCredentials(t *testing.T) {
	const credentialSentinel = "credential-that-must-not-leak"
	_, err := Start(context.Background(), "socks5://user:"+credentialSentinel+"@proxy.example:1080")
	if err == nil {
		t.Fatal("authenticated SOCKS proxy unexpectedly accepted")
	}
	if strings.Contains(err.Error(), credentialSentinel) {
		t.Fatalf("error disclosed proxy credentials: %v", err)
	}
}

func TestBridgeCloseTerminatesActiveConnectTunnel(t *testing.T) {
	upstreamClosed := make(chan struct{})
	upstreamProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = fmt.Fprint(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go func() {
			defer close(upstreamClosed)
			defer conn.Close()
			_, _ = io.Copy(io.Discard, conn)
		}()
	}))
	defer upstreamProxy.Close()

	rawProxy := "http://user:pass@" + strings.TrimPrefix(upstreamProxy.URL, "http://")
	bridge, err := Start(context.Background(), rawProxy)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	bridgeURL, err := url.Parse(bridge.URL())
	if err != nil {
		t.Fatalf("bridge URL = %v", err)
	}
	client, err := net.DialTimeout("tcp", bridgeURL.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial bridge = %v", err)
	}
	defer client.Close()
	if _, err := fmt.Fprint(client, "CONNECT target.invalid:443 HTTP/1.1\r\nHost: target.invalid:443\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT = %v", err)
	}
	request, _ := http.NewRequest(http.MethodConnect, "http://target.invalid:443", nil)
	response, err := http.ReadResponse(bufio.NewReader(client), request)
	if err != nil {
		t.Fatalf("read CONNECT response = %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", response.StatusCode)
	}

	bridge.Close()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("client tunnel remained readable after bridge.Close")
	} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("client tunnel remained open after bridge.Close")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream tunnel remained open after bridge.Close")
	}
}
