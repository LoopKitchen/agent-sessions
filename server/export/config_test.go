package export

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestLoadNamesEveryMissingVariableAtOnce: the job's environment is fixed
// once in job.yaml, so a failure has to list everything wrong with it.
func TestLoadNamesEveryMissingVariableAtOnce(t *testing.T) {
	_, err := Load(envOf(map[string]string{}))
	if err == nil {
		t.Fatal("loaded with nothing set")
	}
	for _, want := range []string{"DATABASE_HOST", "DATABASE_USER", "DATABASE_PASSWORD", "EXPORT_PROJECT", "EXPORT_BUCKET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
	_, err = Load(envOf(map[string]string{
		"DATABASE_HOST": "/cloudsql/x", "DATABASE_USER": "u", "DATABASE_PASSWORD": "p",
		"EXPORT_PROJECT": "proj", "EXPORT_BUCKET": "b",
		"EXPORT_MAX_SESSIONS": "0", "EXPORT_MAX_EVENT_DAYS": "many", "LOG_LEVEL": "loud", "DATABASE_PORT": "99999",
	}))
	if err == nil {
		t.Fatal("loaded with bad numbers")
	}
	for _, want := range []string{"EXPORT_MAX_SESSIONS", "EXPORT_MAX_EVENT_DAYS", "LOG_LEVEL", "DATABASE_PORT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

// TestLoadAppliesTheDefaultsAndReportsSecretsAsPresent pins the defaults
// job.yaml relies on and the LogValue shape (no password, ever).
func TestLoadAppliesTheDefaultsAndReportsSecretsAsPresent(t *testing.T) {
	cfg, err := Load(envOf(map[string]string{
		"DATABASE_HOST": "/cloudsql/x", "DATABASE_USER": "u", "DATABASE_PASSWORD": "hunter2",
		"EXPORT_PROJECT": "proj", "EXPORT_BUCKET": "b", "LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	// The default dataset is the raw one the loads go into, never
	// the governed loop_sessions dataset of views.
	if cfg.DatabasePort != 5432 || cfg.DatabaseName != "loop_sessions" || cfg.Dataset != "loop_sessions_raw" || cfg.Location != "asia-south1" ||
		cfg.MaxSessions != DefaultMaxSessions || cfg.MaxSessionDays != DefaultMaxSessionDays || cfg.MaxEventDays != DefaultMaxEventDays || cfg.LogLevel != slog.LevelDebug {
		t.Errorf("defaults: %+v", cfg)
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("cfg", "config", cfg)
	if strings.Contains(buf.String(), "hunter2") || !strings.Contains(buf.String(), `"database_password":"set"`) || !strings.Contains(buf.String(), `"token":"metadata"`) {
		t.Errorf("log line: %s", buf.String())
	}
	pc, err := cfg.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	if pc.MaxConns != 2 || pc.MinConns != 0 || pc.ConnConfig.Password != "hunter2" || pc.ConnConfig.Host != "/cloudsql/x" || pc.AfterConnect == nil {
		t.Errorf("pool config: max %d min %d host %q", pc.MaxConns, pc.MinConns, pc.ConnConfig.Host)
	}
}

// TestMainRefusesToStartOnAnEnvironmentItCannotUse mirrors the server's
// own entrypoint test: nothing is opened when the configuration is wrong,
// and the line says what is missing.
func TestMainRefusesToStartOnAnEnvironmentItCannotUse(t *testing.T) {
	var out bytes.Buffer
	err := Main(context.Background(), envOf(map[string]string{}), &out, "test")
	if err == nil {
		t.Fatal("Main ran with nothing configured")
	}
	if !strings.Contains(out.String(), `"message":"cannot start"`) || !strings.Contains(out.String(), "EXPORT_BUCKET") || !strings.Contains(out.String(), `"job":"export"`) {
		t.Errorf("log: %s", out.String())
	}
}
