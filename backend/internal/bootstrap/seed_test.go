package bootstrap

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestStartupSeedDoesNotReintroduceLegacyCatalog(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	raw, err := os.ReadFile(strings.TrimSuffix(filename, "_test.go") + ".go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, forbidden := range []string{"model_configs", "lumina-", "firefly-", "credits.", "pay."} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("startup seed contains retired product data %q", forbidden)
		}
	}
}
