// Package config loads cocote's runtime configuration from environment
// variables, with a best-effort fallback to a .env file. Real environment
// variables always take precedence over values found in .env.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds everything cocote needs to run.
type Config struct {
	BotToken    string        // Telegram bot token (COCOTE_BOT_TOKEN)
	ChatID      int64         // Target chat id cocote talks to (COCOTE_CHAT_ID)
	SessionName string        // Human label for this Claude Code session (COCOTE_SESSION_NAME)
	LeaseTTL    time.Duration // How long a booking lease stays valid without a heartbeat (COCOTE_LEASE_TTL)
	PollTimeout time.Duration // Long-poll timeout for getUpdates (COCOTE_POLL_TIMEOUT)
	LeaseDir    string        // Directory holding the booking lock (COCOTE_LEASE_DIR)
}

// Load reads configuration from the environment (and .env), validating the
// required fields.
func Load() (*Config, error) {
	loadDotEnvFiles()

	token := strings.TrimSpace(os.Getenv("COCOTE_BOT_TOKEN"))
	if token == "" {
		return nil, errors.New("COCOTE_BOT_TOKEN is required (set it as an env var or in .env)")
	}

	chatStr := strings.TrimSpace(os.Getenv("COCOTE_CHAT_ID"))
	if chatStr == "" {
		return nil, errors.New("COCOTE_CHAT_ID is required (set it as an env var or in .env)")
	}
	chatID, err := strconv.ParseInt(chatStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("COCOTE_CHAT_ID must be a numeric Telegram chat id: %w", err)
	}

	session := strings.TrimSpace(os.Getenv("COCOTE_SESSION_NAME"))
	if session == "" {
		host, herr := os.Hostname()
		if herr != nil || host == "" {
			host = "claude-code"
		}
		session = fmt.Sprintf("%s-%d", host, os.Getpid())
	}

	leaseDir := strings.TrimSpace(os.Getenv("COCOTE_LEASE_DIR"))
	if leaseDir == "" {
		base, derr := os.UserConfigDir()
		if derr != nil || base == "" {
			base = os.TempDir()
		}
		leaseDir = filepath.Join(base, "cocote")
	}

	return &Config{
		BotToken:    token,
		ChatID:      chatID,
		SessionName: session,
		LeaseTTL:    durEnv("COCOTE_LEASE_TTL", 30*time.Second),
		PollTimeout: durEnv("COCOTE_POLL_TIMEOUT", 30*time.Second),
		LeaseDir:    leaseDir,
	}, nil
}

// durEnv parses a duration from the environment. It accepts Go duration
// strings ("45s", "2m") as well as a bare number of seconds ("30").
func durEnv(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return def
}

// loadDotEnvFiles loads .env from COCOTE_ENV_FILE, the working directory, and
// the executable's directory (in that order). Existing env vars win.
func loadDotEnvFiles() {
	var paths []string
	if p := strings.TrimSpace(os.Getenv("COCOTE_ENV_FILE")); p != "" {
		paths = append(paths, p)
	}
	if wd, err := os.Getwd(); err == nil {
		paths = append(paths, filepath.Join(wd, ".env"))
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), ".env"))
	}
	for _, p := range paths {
		loadDotEnv(p)
	}
}

func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}
