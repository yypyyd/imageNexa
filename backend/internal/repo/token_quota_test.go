package repo

import (
	"encoding/json"
	"testing"

	"gorm.io/datatypes"
)

func TestQuotaTenthsKeepsBytePlusFractionalCostsExact(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  int64
	}{
		{value: 0.3, want: 3},
		{value: 3.5, want: 35},
		{value: 9.9, want: 99},
		{value: 230, want: 2300},
	} {
		got, ok := quotaTenths(tc.value)
		if !ok || got != tc.want {
			t.Fatalf("quotaTenths(%v) = %d, %v; want %d, true", tc.value, got, ok, tc.want)
		}
	}
}

func TestMetaTenthsAcceptsDatabaseNumberRepresentations(t *testing.T) {
	for _, value := range []any{3.5, "3.5", json.Number("3.5")} {
		got, ok := metaTenths(datatypes.JSONMap{"cached_quota_remaining": value}, "cached_quota_remaining")
		if !ok || got != 35 {
			t.Fatalf("metaTenths(%T(%v)) = %d, %v; want 35, true", value, value, got, ok)
		}
	}
	if got := canonicalTenths(35); got != 3.5 {
		t.Fatalf("canonicalTenths(35) = %#v, want 3.5", got)
	}
	if got := canonicalTenths(120); got != 12 {
		t.Fatalf("canonicalTenths(120) = %#v, want 12", got)
	}
}
