package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckGroups(t *testing.T) {
	expected := []string{"agents", "research"}
	// FreeIPA user private group appears in NSS as the persona's own name.
	if err := checkGroups([]string{"alice-ai", "agents", "research", "ipausers"}, expected, "alice-ai"); err != nil {
		t.Fatal(err)
	}
	for _, actual := range [][]string{
		{"alice-ai", "agents"},                            // missing research
		{"alice-ai", "agents", "research", "restricted"}, // nested/stale extra group
		{"bob-ai", "agents", "research"},                  // someone else's private group
	} {
		if checkGroups(actual, expected, "alice-ai") == nil {
			t.Errorf("accepted %v", actual)
		}
	}
}

func TestShareHome(t *testing.T) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("needs an existing persona-like user")
	}
	human := &user.User{Username: current.Username, HomeDir: t.TempDir()}
	if err := shareHome(human, "nobody", []string{".claude", ".config/opencode"}); err != nil {
		t.Fatal(err)
	}
	acl := func(rel string) string {
		out, err := exec.Command("getfacl", "-p", filepath.Join(human.HomeDir, rel)).CombinedOutput()
		if err != nil {
			t.Fatalf("getfacl %s: %v %s", rel, err, out)
		}
		return string(out)
	}
	if a := acl("."); !strings.Contains(a, "user:nobody:--x") || strings.Contains(a, "default:") {
		t.Fatalf("HOME must be traverse-only:\n%s", a)
	}
	if a := acl(".config"); !strings.Contains(a, "user:nobody:--x") {
		t.Fatalf("intermediate dir must be traverse-only:\n%s", a)
	}
	for _, rel := range []string{".claude", ".config/opencode"} {
		a := acl(rel)
		if !strings.Contains(a, "user:nobody:rwx") || !strings.Contains(a, "default:user:nobody:rwx") {
			t.Fatalf("%s not shared read-write:\n%s", rel, a)
		}
	}

	evil := &user.User{Username: current.Username, HomeDir: t.TempDir()}
	if err := os.Symlink(t.TempDir(), filepath.Join(evil.HomeDir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := shareHome(evil, "nobody", []string{".claude"}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked shared path accepted: %v", err)
	}
	if shareHome(human, "nobody", nil) == nil {
		t.Fatal("empty shared paths accepted")
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
