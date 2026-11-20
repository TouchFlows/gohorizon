package redis

import "testing"

func TestKeysMatchDocumentedLayout(t *testing.T) {
	k, err := NewKeys("production", "production")
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}

	// These exact strings are asserted again in packages/laravel/tests/KeysTest.php.
	// Changing them is a cross-language protocol change, not a refactor.
	if got, want := k.Events(), "gohorizon:production:production:events"; got != want {
		t.Errorf("Events() = %q, want %q", got, want)
	}
	if got, want := k.SupervisorLock(), "gohorizon:production:production:coord:lock:supervisor"; got != want {
		t.Errorf("SupervisorLock() = %q, want %q", got, want)
	}
	if got, want := k.Coordinates(), "gohorizon:production:production:coord:"; got != want {
		t.Errorf("Coordinates() = %q, want %q", got, want)
	}
	sup, err := k.SupervisorControl("default-supervisor")
	if err != nil {
		t.Fatalf("SupervisorControl: %v", err)
	}
	if got, want := sup, "gohorizon:production:production:coord:supervisor:default-supervisor"; got != want {
		t.Errorf("SupervisorControl() = %q, want %q", got, want)
	}
}

func TestKeysRejectUnsafeSegments(t *testing.T) {
	cases := []struct{ app, env string }{
		{"", "production"},
		{"Production", "production"},
		{"pro duction", "production"},
		{"prod:uction", "production"},
		{"{production}", "production"},
		{"production", "prod\nuction"},
		{"production", "-production"},
	}
	for _, tc := range cases {
		if _, err := NewKeys(tc.app, tc.env); err == nil {
			t.Errorf("NewKeys(%q, %q) accepted an unsafe segment", tc.app, tc.env)
		}
	}
}

func TestSupervisorControlRejectsUnsafeID(t *testing.T) {
	k, err := NewKeys("app", "production")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "Supervisor", "a:b", "a b"} {
		if _, err := k.SupervisorControl(id); err == nil {
			t.Errorf("SupervisorControl(%q) accepted an unsafe id", id)
		}
	}
}

func TestEventsKeyStaysInsideScope(t *testing.T) {
	k, _ := NewKeys("app", "production")
	for _, key := range []string{k.Events(), k.SupervisorLock(), k.Coordinates()} {
		if len(key) <= len(k.Scope()) || key[:len(k.Scope())] != k.Scope() {
			t.Errorf("key %q escapes scope %q", key, k.Scope())
		}
	}
}
