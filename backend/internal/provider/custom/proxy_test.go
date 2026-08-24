package custom

import (
	"net/http"
	"testing"
)

func TestClientAlwaysUsesDirectEgress(t *testing.T) {
	client := NewClient()
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.http.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("custom client must ignore environment and configured proxies")
	}
}
