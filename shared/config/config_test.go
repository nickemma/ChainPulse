package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := "" +
		"LOG_LEVEL=debug          # inline comment must be stripped\n" +
		"# a full-line comment\n" +
		"\n" +
		"REDIS_PASSWORD=\"p@ss#word\"  # quoted value keeps its hash\n" +
		"PREEXISTING=from_file\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	// A real env var must win over the file value.
	t.Setenv("PREEXISTING", "from_env")
	// Ensure target keys start unset so the file populates them.
	os.Unsetenv("LOG_LEVEL")
	os.Unsetenv("REDIS_PASSWORD")
	t.Cleanup(func() { os.Unsetenv("LOG_LEVEL"); os.Unsetenv("REDIS_PASSWORD") })

	loadDotEnv(path)

	if got := os.Getenv("LOG_LEVEL"); got != "debug" {
		t.Errorf("inline comment not stripped: got %q", got)
	}
	if got := os.Getenv("REDIS_PASSWORD"); got != "p@ss#word" {
		t.Errorf("quoted hash not preserved: got %q", got)
	}
	if got := os.Getenv("PREEXISTING"); got != "from_env" {
		t.Errorf("env var should win over .env file: got %q", got)
	}
}

func TestLoadDotEnvMissingFileIsNoError(t *testing.T) {
	// Must not panic or set anything for a nonexistent file.
	loadDotEnv(filepath.Join(t.TempDir(), "does-not-exist"))
}
