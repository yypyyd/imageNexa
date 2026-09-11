package proxypool

import (
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	minBatch = 16
	maxBatch = 64
)

var queryParamPattern = regexp.MustCompile(`(?i)(^|&)([^=&]+)=([^&]*)`)

// ParseLine turns one extract-API text line into an http/socks proxy URL.
// Supported forms: already-absolute URLs, user:pass@host:port, host:port, and
// host:port:user:pass (password may contain colons).
func ParseLine(line string) string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, `"'`)
	if line == "" || strings.ContainsAny(line, " \t") {
		return ""
	}
	if strings.Contains(line, "://") {
		parsed, err := url.Parse(line)
		if err != nil || parsed.Host == "" {
			return ""
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "socks5", "socks5h":
			return parsed.String()
		default:
			return ""
		}
	}
	if strings.Contains(line, "@") {
		parsed, err := url.Parse("http://" + line)
		if err != nil || parsed.Host == "" || parsed.User == nil {
			return ""
		}
		return parsed.String()
	}
	host, port, user, pass, ok := splitHostPortCreds(line)
	if !ok {
		return ""
	}
	if user == "" {
		return "http://" + net.JoinHostPort(host, port)
	}
	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, port),
		User:   url.UserPassword(user, pass),
	}
	return u.String()
}

func splitHostPortCreds(line string) (host, port, user, pass string, ok bool) {
	parts := strings.Split(line, ":")
	if len(parts) == 2 && validPort(parts[1]) && parts[0] != "" {
		return parts[0], parts[1], "", "", true
	}
	if len(parts) >= 4 && validPort(parts[1]) && parts[0] != "" && parts[2] != "" {
		return parts[0], parts[1], parts[2], strings.Join(parts[3:], ":"), true
	}
	return "", "", "", "", false
}

func validPort(raw string) bool {
	port, err := strconv.Atoi(raw)
	return err == nil && port > 0 && port <= 65535
}

func batchCount(endpoint string, needed int) int {
	capAt := maxBatch
	if n := queryInt(endpoint, "count"); n > 0 && n < capAt {
		capAt = n
	}
	if needed < 1 {
		needed = minBatch
	}
	if needed < minBatch && capAt >= minBatch {
		needed = minBatch
	}
	if needed > capAt {
		return capAt
	}
	return needed
}

func leaseTTL(endpoint string) time.Duration {
	n := queryInt(endpoint, "sessTime", "sesstime", "sess_time")
	var d time.Duration
	switch {
	case n <= 0:
		d = 15 * time.Minute
	case n > 180:
		d = time.Duration(n) * time.Second
	default:
		d = time.Duration(n) * time.Minute
	}
	if d < 2*time.Minute {
		d = 2 * time.Minute
	}
	return d * 9 / 10
}

func queryInt(rawURL string, names ...string) int {
	values := rawQuery(rawURL)
	for _, name := range names {
		for key, value := range values {
			if strings.EqualFold(key, name) {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func rawQuery(rawURL string) map[string]string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	matches := queryParamPattern.FindAllStringSubmatch(parsed.RawQuery, -1)
	for _, match := range matches {
		if len(match) != 4 {
			continue
		}
		key, err := url.QueryUnescape(match[2])
		if err != nil {
			key = match[2]
		}
		value, err := url.QueryUnescape(match[3])
		if err != nil {
			value = match[3]
		}
		out[key] = value
	}
	return out
}

func setQueryParam(rawURL, key, value string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return rawURL
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return rawURL
	}
	escapedKey := url.QueryEscape(key)
	escapedValue := url.QueryEscape(value)
	re := regexp.MustCompile(`(?i)(^|&)(` + regexp.QuoteMeta(key) + `|` + regexp.QuoteMeta(escapedKey) + `)=([^&]*)`)
	query := parsed.RawQuery
	if re.MatchString(query) {
		parsed.RawQuery = re.ReplaceAllString(query, "${1}"+escapedKey+"="+escapedValue)
		return parsed.String()
	}
	if query == "" {
		parsed.RawQuery = escapedKey + "=" + escapedValue
	} else {
		parsed.RawQuery = query + "&" + escapedKey + "=" + escapedValue
	}
	return parsed.String()
}

func rewriteRegion(endpoint, region string) string {
	region = strings.ToUpper(strings.TrimSpace(region))
	if endpoint == "" || region == "" {
		return endpoint
	}
	return setQueryParam(endpoint, "region", region)
}

func prepareFetchURL(endpoint string, count int) string {
	if count < 1 {
		count = 1
	}
	return setQueryParam(endpoint, "count", strconv.Itoa(count))
}
