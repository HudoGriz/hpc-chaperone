package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckGroups(t *testing.T) {
	if err := checkGroups([]string{"alice-ai", "ipausers"}, "alice-ai"); err != nil {
		t.Fatal(err)
	}
	for _, actual := range [][]string{
		{"ipausers"},             // own group missing
		{"alice-ai", "research"}, // any other group
		{"alice-ai", "bob-ai"},   // someone else's persona group
	} {
		if checkGroups(actual, "alice-ai") == nil {
			t.Errorf("accepted %v", actual)
		}
	}
}

func TestSeedClaudeConfig(t *testing.T) {
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, ".claude"), 0700)
	seedClaudeConfig(home) // nothing to seed
	if _, err := os.Stat(filepath.Join(home, ".claude", ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("seeded without a source")
	}
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark"}`), 0600)
	seedClaudeConfig(home)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"light"}`), 0600)
	seedClaudeConfig(home) // once only: the persona's later changes win
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", ".claude.json")); string(b) != `{"theme":"dark"}` {
		t.Fatalf("seed: %s", b)
	}
}

func TestPolicyOverrideOnlyForInspection(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("override is ignored for root")
	}
	t.Setenv("CHAPERONE_POLICY", "config/policy.example.json")
	if err := runClient([]string{"true"}); err == nil || !strings.Contains(err.Error(), "CHAPERONE_POLICY") {
		t.Fatalf("user-selected policy used for a real launch: %v", err)
	}
	if err := runClient([]string{"--list-personas"}); err != nil {
		t.Fatalf("inspection with override failed: %v", err)
	}
}

func TestExitStatus(t *testing.T) {
	for script, want := range map[string]int{"exit 7": 7, "kill -TERM $$": 143, "true": 0} {
		cmd := exec.Command("sh", "-c", script)
		cmd.Run()
		if got := exitStatus(cmd.ProcessState); got != want {
			t.Errorf("%q: exit %d, want %d", script, got, want)
		}
	}
}

func TestPrivilegedEntrypointsGuard(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("guards differ for root")
	}
	if err := runEnter([]string{"--persona", "ai", "--path", "/bin", "--", "id"}); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("_enter ran without root: %v", err)
	}
	if err := runServer([]string{"--listen"}); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("provisiond accepted bad arguments: %v", err)
	}
}

func TestClientInspection(t *testing.T) {
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs non-root NSS user")
	}
	p := examplePolicy()
	write := func(p Policy) string {
		b, _ := json.Marshal(p)
		path := filepath.Join(t.TempDir(), "policy.json")
		os.WriteFile(path, b, 0600)
		return path
	}
	t.Setenv("CHAPERONE_POLICY", write(p))
	if err := runClient([]string{"-p", "nope", "--status"}); err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Fatalf("unknown persona: %v", err)
	}
	for _, args := range [][]string{{"--status"}, {"-n", "-p", "ai-special", "--", "codex", "x y"}, {"--help"}, {"-p", "ai", "-h"}} {
		if err := runClient(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
