package model

import (
	"reflect"
	"testing"
)

func TestAutoMigrateModelsExcludesMigrationOwnedCoreTables(t *testing.T) {
	want := []reflect.Type{
		reflect.TypeOf(&BannedWord{}),
		reflect.TypeOf(&BannedWordHit{}),
		reflect.TypeOf(&EventLog{}),
		reflect.TypeOf(&RefreshProfile{}),
		reflect.TypeOf(&SiteSetting{}),
	}
	gotModels := AutoMigrateModels()
	if len(gotModels) != len(want) {
		t.Fatalf("AutoMigrateModels() returned %d models, want %d", len(gotModels), len(want))
	}
	for index, model := range gotModels {
		if got := reflect.TypeOf(model); got != want[index] {
			t.Fatalf("AutoMigrateModels()[%d] = %v, want %v", index, got, want[index])
		}
	}
}
