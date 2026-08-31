package bootstrap

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestSeedBytePlusModelsAndCapabilityOnlyBackfill(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test source")
	}
	raw, err := os.ReadFile(strings.TrimSuffix(filename, "_byteplus_test.go") + ".go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)

	insert := capturedSeedStatement(source, "INSERT INTO model_configs", "lumina-seedream-5.0-pro")
	backfill := capturedSeedStatement(source, "UPDATE model_configs AS m SET", "lumina-seedream-5.0-pro")
	if insert == "" || backfill == "" {
		t.Fatalf("missing BytePlus seed SQL: insert=%t backfill=%t", insert != "", backfill != "")
	}

	models := map[string]string{
		"lumina-seedream-5.0-pro":  "7657401949175693322",
		"lumina-gpt-image-2":       "6824519374061285743",
		"lumina-seedream-5.0-lite": "7604761017696141358",
		"lumina-nano-banana-2":     "8162745039814627354",
		"lumina-nano-banana-pro":   "8162745039814627353",
	}
	for id, upstream := range models {
		if strings.Count(insert, "'"+id+"'") != 1 || strings.Count(backfill, "'"+id+"'") != 1 {
			t.Errorf("model %s must appear exactly once in insert and backfill", id)
		}
		if !strings.Contains(insert, "'"+upstream+"'") || !strings.Contains(backfill, "'"+upstream+"'") {
			t.Errorf("model %s missing upstream service id %s", id, upstream)
		}
	}
	for _, priceTiers := range []string{
		`'{"1K":0,"2K":0}'::jsonb`,
		`'{"2K":0}'::jsonb`,
		`'{"1K":0,"2K":0,"4K":0}'::jsonb`,
	} {
		if !strings.Contains(insert, priceTiers) {
			t.Errorf("insert missing zero-price tiers %s", priceTiers)
		}
	}
	if !strings.Contains(insert, "ON CONFLICT (id) DO NOTHING") {
		t.Error("BytePlus insert must preserve existing rows with ON CONFLICT DO NOTHING")
	}

	setClause := strings.SplitN(backfill, "FROM (VALUES", 2)[0]
	for _, protected := range []string{"prices =", "prices_agent =", "enabled =", "alias =", "weight =", "generation_count ="} {
		if strings.Contains(setClause, protected) {
			t.Errorf("capability backfill overwrites administrator-controlled field: %s", protected)
		}
	}
}

func capturedSeedStatement(source, prefix, modelID string) string {
	start := strings.Index(source, prefix)
	for start >= 0 {
		rest := source[start:]
		end := strings.Index(rest, "`).Error")
		if end < 0 {
			return ""
		}
		statement := rest[:end]
		if strings.Contains(statement, modelID) {
			return statement
		}
		next := strings.Index(rest[len(prefix):], prefix)
		if next < 0 {
			return ""
		}
		start += len(prefix) + next
	}
	return ""
}
