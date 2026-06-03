package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFromEnvFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	contents := "" +
		"# cocote test env\n" +
		"COCOTE_BOT_TOKEN=\"123:ABC\"\n" +
		"export COCOTE_CHAT_ID=42\n" +
		"COCOTE_SESSION_NAME=worker-7\n" +
		"COCOTE_LEASE_TTL=45s\n" +
		"COCOTE_POLL_TIMEOUT=20\n"
	if err := os.WriteFile(envPath, []byte(contents), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}

	// Point Load at our file and make sure no real vars are set.
	t.Setenv("COCOTE_ENV_FILE", envPath)
	for _, k := range []string{"COCOTE_BOT_TOKEN", "COCOTE_CHAT_ID", "COCOTE_SESSION_NAME", "COCOTE_LEASE_TTL", "COCOTE_POLL_TIMEOUT", "COCOTE_LEASE_DIR"} {
		os.Unsetenv(k)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "123:ABC" {
		t.Errorf("BotToken = %q, want 123:ABC", cfg.BotToken)
	}
	if cfg.ChatID != 42 {
		t.Errorf("ChatID = %d, want 42", cfg.ChatID)
	}
	if cfg.SessionName != "worker-7" {
		t.Errorf("SessionName = %q, want worker-7", cfg.SessionName)
	}
	if cfg.LeaseTTL != 45*time.Second {
		t.Errorf("LeaseTTL = %v, want 45s", cfg.LeaseTTL)
	}
	if cfg.PollTimeout != 20*time.Second {
		t.Errorf("PollTimeout = %v, want 20s (bare seconds)", cfg.PollTimeout)
	}
}

func TestRealEnvWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("COCOTE_BOT_TOKEN=from-file\nCOCOTE_CHAT_ID=1\n"), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}
	t.Setenv("COCOTE_ENV_FILE", envPath)
	t.Setenv("COCOTE_BOT_TOKEN", "from-env")
	os.Unsetenv("COCOTE_CHAT_ID")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BotToken != "from-env" {
		t.Errorf("BotToken = %q, want from-env (real env must win over .env)", cfg.BotToken)
	}
}

func TestMissingRequiredFields(t *testing.T) {
	t.Setenv("COCOTE_ENV_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	os.Unsetenv("COCOTE_BOT_TOKEN")
	os.Unsetenv("COCOTE_CHAT_ID")
	if _, err := Load(); err == nil {
		t.Fatal("Load succeeded with no token/chat id, want error")
	}
}
