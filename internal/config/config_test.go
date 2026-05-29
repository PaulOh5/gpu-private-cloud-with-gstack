package config

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("FLEX_CONFIG", filepath.Join(t.TempDir(), "config.json"))

	// Missing file -> zero config, no error.
	c, err := Load()
	if err != nil || c.Coordinator != "" {
		t.Fatalf("Load() of missing = %+v, %v", c, err)
	}

	want := Config{Coordinator: "http://office-coord.tailnet:7070", Node: "office-a"}
	if err := Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}
