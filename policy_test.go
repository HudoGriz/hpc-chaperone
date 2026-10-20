package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func examplePolicy() Policy {
	return Policy{
		Default: "ai", ProvisionURL: "https://provision.example:9443", EligibleGroups: []string{"chaperone-users"},
		SharedPaths: []string{".claude", ".config/opencode"},
		Personas:    map[string]Persona{"ai": {SlurmAccount: "agents"}, "ai-special": {SlurmAccount: "agents-special"}},
	}
}
func TestValidationRejectsEscalation(t *testing.T) {
	p := examplePolicy()
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	p.SharedPaths = []string{"../.ssh"}
	if p.validate() == nil {
		t.Fatal("escaped path accepted")
	}
	p = examplePolicy()
	p.ProvisionURL = "http://localhost"
	if p.validate() == nil {
		t.Fatal("insecure provisioning transport accepted")
	}
	for _, bad := range []string{".ssh", ".ssh/keys", ".config/gh", ".aws/x", ".netrc", "/etc", "a\\b", ""} {
		p = examplePolicy()
		p.SharedPaths = []string{bad}
		if p.validate() == nil {
			t.Errorf("shared path %q accepted", bad)
		}
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Default = "missing" },
		func(p *Policy) { p.Default = "Bad Name" },
		func(p *Policy) { p.EligibleGroups = []string{"*"} },
		func(p *Policy) { p.Personas["x y"] = Persona{SlurmAccount: "a"} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "bad account"} },
	} {
		p = examplePolicy()
		mutate(&p)
		if p.validate() == nil {
			t.Errorf("invalid policy accepted: %+v", p)
		}
	}
	p = examplePolicy()
	p.EligibleGroups = nil
	if p.eligible(nil) {
		t.Fatal("empty eligibility should deny")
	}
}
func TestStrictJSONPolicy(t *testing.T) {
	p := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(p, []byte(`{"default_persona":"ai","personas":{"ai":{"slurm_account":"agents"}},"typo":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPolicy(p, false); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := os.WriteFile(p, []byte(`{"default_persona":"ai","eligible_groups":["r"],"personas":{"ai":{"slurm_account":"agents"}}}{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPolicy(p, false); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if os.Geteuid() != 0 {
		if _, err := loadPolicy("config/policy.example.json", true); err == nil || !strings.Contains(err.Error(), "root") {
			t.Fatalf("non-root-owned site policy accepted: %v", err)
		}
	}
}
func TestProfileName(t *testing.T) {
	if got, err := personaName("alice", "ai-special"); err != nil || got != "alice-ai-special" {
		t.Fatalf("%q %v", got, err)
	}
	for _, p := range []string{"../root", "", "AI", "foo/bar", strings.Repeat("a", 25)} {
		if _, err := personaName("alice", p); err == nil {
			t.Errorf("accepted %q", p)
		}
	}
}
func TestParseCLI(t *testing.T) {
	o, err := parseArgs([]string{"-p", "ai-special", "--", "-strange", "x y"}, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if o.profile != "ai-special" || !reflect.DeepEqual(o.command, []string{"-strange", "x y"}) {
		t.Fatalf("%+v", o)
	}
	o, err = parseArgs([]string{"codex", "--model", "gpt-6"}, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.command, []string{"codex", "--model", "gpt-6"}) {
		t.Fatal(o.command)
	}
	if _, err = parseArgs([]string{"-p"}, "ai"); err == nil {
		t.Fatal("missing persona accepted")
	}
	o, err = parseArgs([]string{"--persona=ai-special", "-n", "--status", "--list-personas"}, "ai")
	if err != nil || o.profile != "ai-special" || !o.dry || !o.status || !o.list || o.command != nil {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err = parseArgs([]string{"--unknown"}, "ai"); err == nil {
		t.Fatal("unknown option accepted")
	}
}
func TestIncludedPolicyExample(t *testing.T) {
	p, err := loadPolicy("config/policy.example.json", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != "ai" || len(p.Personas) != 2 {
		t.Fatalf("unexpected example: %+v", p)
	}
}
