package config

import "testing"

func TestParseProjects(t *testing.T) {
	specs, err := ParseProjects("app:tp-pk-a:tp-sk-a, eval:tp-pk-b:tp-sk-b")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 || specs[0].Name != "app" || specs[1].SecretKey != "tp-sk-b" {
		t.Fatalf("specs = %+v", specs)
	}

	if _, err := ParseProjects("app:only-two"); err == nil {
		t.Fatal("expected error for malformed entry")
	}
	if _, err := ParseProjects("app:p:s,app:p2:s2"); err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if specs, err := ParseProjects("  "); err != nil || specs != nil {
		t.Fatalf("blank input: specs=%v err=%v", specs, err)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("TRACEPAD_LISTEN", ":9999")
	cfg, err := Load([]string{"--listen", ":4318"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":4318" {
		t.Fatalf("Listen = %q, flags must override env", cfg.Listen)
	}
}
