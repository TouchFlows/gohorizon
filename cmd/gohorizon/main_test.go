package main

import (
	"testing"

	"github.com/gohorizon/gohorizon/internal/auth"
	"github.com/gohorizon/gohorizon/internal/config"
)

func authConfig(t *testing.T, keyEnv string) *config.Config {
	t.Helper()
	hash, err := auth.HashPassword("s3cret")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return &config.Config{Server: config.Server{Auth: config.Auth{
		SessionKeyEnv: keyEnv,
		SessionHours:  config.DefaultSessionHours,
		Users:         []config.User{{Name: "alice", PasswordHash: hash}},
	}}}
}

func TestNewAuthStoreStaysNilWithoutUsers(t *testing.T) {
	store, err := newAuthStore(&config.Config{})
	if err != nil || store != nil {
		t.Fatalf("store = %v, err = %v; want no store and no error", store, err)
	}
}

func TestNewAuthStoreBuildsAStoreFromTheEnvironment(t *testing.T) {
	t.Setenv("GOHORIZON_TEST_SESSION_KEY", "0123456789abcdef0123456789abcdef")
	store, err := newAuthStore(authConfig(t, "GOHORIZON_TEST_SESSION_KEY"))
	if err != nil {
		t.Fatalf("newAuthStore: %v", err)
	}
	if store == nil {
		t.Fatal("no store for a configured deployment")
	}
}

func TestNewAuthStoreRefusesAMissingKey(t *testing.T) {
	t.Setenv("GOHORIZON_TEST_SESSION_KEY", "")
	if _, err := newAuthStore(authConfig(t, "GOHORIZON_TEST_SESSION_KEY")); err == nil {
		t.Fatal("a configured deployment started without a signing key")
	}
}

func TestNewAuthStoreRefusesAKeyTooShortToSignWith(t *testing.T) {
	t.Setenv("GOHORIZON_TEST_SESSION_KEY", "short")
	if _, err := newAuthStore(authConfig(t, "GOHORIZON_TEST_SESSION_KEY")); err == nil {
		t.Fatal("a signing key shorter than the minimum was accepted")
	}
}
