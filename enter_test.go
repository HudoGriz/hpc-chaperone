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

	// A 0600 create under the default ACL masks the persona out (effective ---);
	// the next launch repairs it without a full re-walk.
	cred := filepath.Join(human.HomeDir, ".claude", ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if a := acl(".claude/.credentials.json"); !strings.Contains(a, "#effective:---") {
		t.Fatalf("expected masked entry before repair:\n%s", a)
	}
	if err := shareHome(human, "nobody", []string{".claude", ".config/opencode"}); err != nil {
		t.Fatal(err)
	}
	if a := acl(".claude/.credentials.json"); !strings.Contains(a, "user:nobody:rw-") || strings.Contains(a, "#effective") {
		t.Fatalf("0600 file not repaired:\n%s", a)
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
	groups, err := userGroups(current)
	if err != nil {
		t.Skip(err)
	}
	p := examplePolicy()
	write := func(p Policy) string {
		b, _ := json.Marshal(p)
		path := filepath.Join(t.TempDir(), "policy.json")
		os.WriteFile(path, b, 0600)
		return path
	}
	t.Setenv("CHAPERONE_POLICY", write(p))
	if err := runClient([]string{"--status"}); err == nil || !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("ineligible user: %v", err)
	}
	if err := runClient([]string{"-p", "nope", "--status"}); err == nil || !strings.Contains(err.Error(), "unknown persona") {
		t.Fatalf("unknown persona: %v", err)
	}
	p.EligibleGroups = groups
	t.Setenv("CHAPERONE_POLICY", write(p))
	for _, args := range [][]string{{"--status"}, {"-n", "-p", "ai-special", "--", "codex", "x y"}, {"--help"}, {"-p", "ai", "-h"}} {
		if err := runClient(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
