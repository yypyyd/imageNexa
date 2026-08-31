package netguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	MaxImageBytes int64 = 64 << 20
	MaxVideoBytes int64 = 1 << 30
	MaxRedirects        = 5
)

var (
	ErrUnsafeAssetURL = errors.New("unsafe upstream asset URL")
	ErrAssetTooLarge  = errors.New("upstream asset exceeds size limit")
	ErrInvalidMedia   = errors.New("upstream asset is not expected media")
)

type MediaKind string

const (
	MediaImage MediaKind = "image"
	MediaVideo MediaKind = "video"
	MediaAny   MediaKind = "media"
)

// ValidateAssetURL accepts only public HTTPS endpoints on the default port.
// allowedHosts entries match either an exact host or its subdomains; nil means
// any public hostname. DNS is checked on every call, including redirects.
func ValidateAssetURL(ctx context.Context, raw string, allowedHosts []string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" {
		return nil, ErrUnsafeAssetURL
	}
	if parsed.User != nil || parsed.Port() != "" && parsed.Port() != "443" {
		return nil, ErrUnsafeAssetURL
	}
	host := strings.ToLower(strings.TrimSuffix(parsed.Hostname(), "."))
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || !hostAllowed(host, allowedHosts) {
		return nil, ErrUnsafeAssetURL
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return nil, ErrUnsafeAssetURL
		}
		return parsed, nil
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("%w: hostname resolution failed", ErrUnsafeAssetURL)
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			return nil, ErrUnsafeAssetURL
		}
	}
	return parsed, nil
}

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		candidate = strings.ToLower(strings.Trim(strings.TrimSpace(candidate), "."))
		if candidate != "" && (host == candidate || strings.HasSuffix(host, "."+candidate)) {
			return true
		}
	}
	return false
}

func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "64:ff9b:1::/48", "100::/64", "2001::/32",
	"2001:2::/48", "2001:10::/28", "2001:20::/28", "2001:db8::/32",
	"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
)

func mustPrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}

func CheckRedirect(allowedHosts []string) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return errors.New("too many upstream asset redirects")
		}
		_, err := ValidateAssetURL(req.Context(), req.URL.String(), allowedHosts)
		return err
	}
}

// HardenHTTPClient pins each connection to the exact public IPs returned by
// the validation lookup performed inside DialContext. This closes the DNS
// rebind gap between a preflight lookup and Transport's later connection while
// preserving the original hostname for TLS SNI and certificate verification.
func HardenHTTPClient(client *http.Client, allowedHosts []string) error {
	if client == nil {
		return errors.New("nil HTTP client")
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == nil {
		return errors.New("HTTP client transport cannot be hardened")
	}
	clone := transport.Clone()
	if clone.Proxy != nil {
		return errors.New("pinned artifact client cannot use a forward proxy")
	}
	clone.DialContext = PinnedDialContext(allowedHosts)
	client.Transport = clone
	client.CheckRedirect = CheckRedirect(allowedHosts)
	return nil
}

func PinnedDialContext(allowedHosts []string) func(context.Context, string, string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || !hostAllowed(strings.ToLower(strings.TrimSuffix(host, ".")), allowedHosts) {
			return nil, ErrUnsafeAssetURL
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, ErrUnsafeAssetURL
		}
		for _, candidate := range addresses {
			if !publicIP(candidate.IP) {
				return nil, ErrUnsafeAssetURL
			}
		}
		var lastErr error
		for _, candidate := range addresses {
			connection, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if dialErr == nil {
				return connection, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}
}

// ResolveRedirect validates one Location without inheriting credentials. The
// caller decides whether same-host credentials may be reattached.
func ResolveRedirect(ctx context.Context, current *url.URL, location string, allowedHosts []string) (*url.URL, error) {
	next, err := current.Parse(strings.TrimSpace(location))
	if err != nil {
		return nil, ErrUnsafeAssetURL
	}
	return ValidateAssetURL(ctx, next.String(), allowedHosts)
}

// GuardResponse performs Content-Length and magic/MIME checks before exposing
// a bounded stream. The returned reader raises ErrAssetTooLarge if a chunked
// response exceeds maxBytes after streaming begins.
func GuardResponse(resp *http.Response, kind MediaKind, maxBytes int64) (io.ReadCloser, string, error) {
	if resp == nil || resp.Body == nil {
		return nil, "", ErrInvalidMedia
	}
	return GuardStream(resp.Body, resp.Header.Get("Content-Type"), resp.ContentLength, kind, maxBytes)
}

func GuardStream(body io.ReadCloser, rawContentType string, contentLength int64, kind MediaKind, maxBytes int64) (io.ReadCloser, string, error) {
	if body == nil {
		return nil, "", ErrInvalidMedia
	}
	if maxBytes <= 0 {
		body.Close()
		return nil, "", ErrAssetTooLarge
	}
	if contentLength > maxBytes {
		body.Close()
		return nil, "", ErrAssetTooLarge
	}
	prefix := make([]byte, 512)
	n, readErr := io.ReadFull(body, prefix)
	if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
		body.Close()
		return nil, "", readErr
	}
	prefix = prefix[:n]
	if len(prefix) == 0 {
		body.Close()
		return nil, "", ErrInvalidMedia
	}
	detected := DetectMediaType(prefix)
	headerType := strings.ToLower(strings.TrimSpace(strings.Split(rawContentType, ";")[0]))
	if parsed, _, err := mime.ParseMediaType(rawContentType); err == nil && parsed != "" {
		headerType = strings.ToLower(parsed)
	}
	if !mediaAllowed(kind, detected) || headerType != "" && headerType != "application/octet-stream" && headerType != "binary/octet-stream" && !mediaAllowed(kind, headerType) {
		body.Close()
		return nil, "", ErrInvalidMedia
	}
	// The bytes are authoritative. CDNs and object stores occasionally retain a
	// stale Content-Type (for example image/png for an AVIF object); allowing the
	// header to overwrite magic detection makes the stored object extension,
	// RustFS metadata, and downstream response disagree. A non-media header still
	// fails closed above, while an allowed-but-different media header is ignored.
	contentType := detected
	reader := io.MultiReader(bytes.NewReader(prefix), body)
	return &boundedReadCloser{reader: reader, closer: body, remaining: maxBytes}, contentType, nil
}

// DetectMediaType returns the media type identified from the payload magic.
// It intentionally does not consult a filename or HTTP header, so callers can
// use the same authoritative result for object extensions and metadata.
func DetectMediaType(prefix []byte) string {
	detected := strings.ToLower(strings.TrimSpace(strings.Split(http.DetectContentType(prefix), ";")[0]))
	if len(prefix) < 12 || !bytes.Equal(prefix[4:8], []byte("ftyp")) {
		return detected
	}
	brand := string(prefix[8:12])
	switch brand {
	case "avif", "avis":
		return "image/avif"
	case "heic", "heix", "hevc", "hevx", "mif1", "msf1":
		return "image/heic"
	default:
		return detected
	}
}

// MediaExtension maps an authoritative media type to its canonical object-key
// extension. The bool is false for types this service should not persist.
func MediaExtension(contentType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/png":
		return ".png", true
	case "image/jpeg":
		return ".jpg", true
	case "image/webp":
		return ".webp", true
	case "image/avif":
		return ".avif", true
	case "image/heic", "image/heif":
		return ".heic", true
	case "image/gif":
		return ".gif", true
	case "video/mp4":
		return ".mp4", true
	case "video/webm":
		return ".webm", true
	case "video/quicktime":
		return ".mov", true
	default:
		return "", false
	}
}

func mediaAllowed(kind MediaKind, contentType string) bool {
	isImage := strings.HasPrefix(contentType, "image/") && contentType != "image/svg+xml"
	isVideo := strings.HasPrefix(contentType, "video/")
	switch kind {
	case MediaImage:
		return isImage
	case MediaVideo:
		return isVideo
	default:
		return isImage || isVideo
	}
}

type boundedReadCloser struct {
	reader    io.Reader
	closer    io.Closer
	remaining int64
	checked   bool
}

func (r *boundedReadCloser) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(p)) > r.remaining {
			p = p[:r.remaining]
		}
		n, err := r.reader.Read(p)
		r.remaining -= int64(n)
		return n, err
	}
	if r.checked {
		return 0, io.EOF
	}
	r.checked = true
	var extra [1]byte
	n, err := r.reader.Read(extra[:])
	if n > 0 {
		return 0, ErrAssetTooLarge
	}
	if err == nil {
		return 0, io.EOF
	}
	return 0, err
}

func (r *boundedReadCloser) Close() error { return r.closer.Close() }
