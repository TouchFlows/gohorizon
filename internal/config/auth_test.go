package config

import (
	"strings"
	"testing"

	"github.com/gohorizon/gohorizon/internal/auth"
)

func withAuth(t *testing.T, block string) string {
	t.Helper()
	return strings.Replace(minimal, "  metrics: true\n", "  metrics: true\n"+block, 1)
}

func mustPasswordHash(t *testing.T) string {
	t.Helper()
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return hash
}

func TestParseReadsUsersAndDefaultsTheSessionLength(t *testing.T) {
	hash := mustPasswordHash(t)
	block := "  auth:\n" +
		"    session_key_env: GOHORIZON_SESSION_KEY\n" +
		"    users:\n" +
		"      - name: alice\n" +
		"        password_hash: '" + hash + "'\n"

	cfg, err := Parse([]byte(withAuth(t, block)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !cfg.Server.Auth.Enabled() {
		t.Fatal("auth with a user is not enabled")
	}
	if cfg.Server.Auth.SessionHours != DefaultSessionHours {
		t.Fatalf("session hours = %d, want %d", cfg.Server.Auth.SessionHours, DefaultSessionHours)
	}
	if cfg.Server.Auth.Users[0].Name != "alice" || cfg.Server.Auth.Users[0].PasswordHash != hash {
		t.Fatalf("user = %+v, want alice with the hash it was given", cfg.Server.Auth.Users[0])
	}
}

func TestParseDefaultsAndReadsTheUserRole(t *testing.T) {
	hash := mustPasswordHash(t)
	block := "  auth:\n" +
		"    session_key_env: GOHORIZON_SESSION_KEY\n" +
		"    users:\n" +
		"      - name: alice\n" +
		"        password_hash: '" + hash + "'\n" +
		"      - name: bob\n" +
		"        password_hash: '" + hash + "'\n" +
		"        role: operator\n"

	cfg, err := Parse([]byte(withAuth(t, block)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Server.Auth.Users[0].Role; got != auth.RoleViewer {
		t.Errorf("alice's role = %q, want the default %q", got, auth.RoleViewer)
	}
	if got := cfg.Server.Auth.Users[1].Role; got != auth.RoleOperator {
		t.Errorf("bob's role = %q, want %q", got, auth.RoleOperator)
	}
}

func TestValidateRejectsBrokenAuthConfiguration(t *testing.T) {
	hash := mustPasswordHash(t)
	block := func(lines ...string) string {
		return "  auth:\n" + strings.Join(lines, "\n") + "\n"
	}
	user := func(name, encoded string) []string {
		return []string{"    users:", "      - name: " + name, "        password_hash: '" + encoded + "'"}
	}

	cases := []struct {
		name  string
		block string
		want  string
	}{
		{
			name:  "no signing key",
			block: block(append([]string{}, user("alice", hash)...)...),
			want:  "session_key_env",
		},
		{
			name:  "no users but a key",
			block: block("    session_key_env: GOHORIZON_SESSION_KEY"),
			want:  "no users are configured",
		},
		{
			name:  "malformed hash",
			block: block(append([]string{"    session_key_env: GOHORIZON_SESSION_KEY"}, user("alice", "plaintext")...)...),
			want:  "password_hash",
		},
		{
			name: "duplicate name",
			block: block(
				"    session_key_env: GOHORIZON_SESSION_KEY",
				"    users:",
				"      - name: alice",
				"        password_hash: '"+hash+"'",
				"      - name: alice",
				"        password_hash: '"+hash+"'",
			),
			want: "listed twice",
		},
		{
			name: "name with capitals",
			block: block(append([]string{"    session_key_env: GOHORIZON_SESSION_KEY"},
				user("Alice", hash)...)...),
			want: "must be lowercase",
		},
		{
			name: "role this service does not know",
			block: block(append([]string{"    session_key_env: GOHORIZON_SESSION_KEY"},
				"    users:", "      - name: alice", "        password_hash: '"+hash+"'", "        role: admin")...),
			want: "role",
		},
		{
			name: "session length out of range",
			block: block(append([]string{"    session_key_env: GOHORIZON_SESSION_KEY", "    session_hours: 9999"},
				user("alice", hash)...)...),
			want: "session_hours",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(withAuth(t, tc.block)))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("expected a validation error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}
