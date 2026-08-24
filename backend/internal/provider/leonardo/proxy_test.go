package leonardo

import "testing"

func TestProxyClientsUseConfiguredProxy(t *testing.T) {
	client := NewClient("http://%zz")
	if _, err := client.newDirectTLSClient(); err != nil {
		t.Fatalf("direct non-submit client used configured proxy: %v", err)
	}
	if _, err := client.newTLSClientP(true); err == nil {
		t.Fatal("proxy client accepted malformed proxy")
	}
}
