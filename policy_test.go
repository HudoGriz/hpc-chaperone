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
		Default: "ai", ProvisionURL: "https://provision.example:9443", EligibleGroups: []string{"research"},
		DeniedGroups: []string{"restricted", "nas_*"}, SharedPaths: []string{".claude", ".config/opencode"},
		Personas: map[string]Persona{"ai": {Keep: []string{"research", "restricted", "nas_foo"}, Add: []string{"agents"}, SlurmAccount: "agents"}, "ai-special": {Keep: []string{"research", "genome-project"}, Add: []string{"agents"}, SlurmAccount: "agents-special"}},
	}
}
func TestPolicyGroupIntersectionAndDeny(t *testing.T) {
	p := examplePolicy()
	if err := p.validate(); err != nil {
		t.Fatal(err)
	}
	groups, err := p.groups([]string{"research", "restricted", "nas_foo", "genome-project"}, "ai")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groups, []string{"agents", "research"}) {
		t.Fatalf("wrong groups %v", groups)
	}
	groups, err = p.groups([]string{"research"}, "ai-special")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groups, []string{"agents", "research"}) {
		t.Fatalf("special gained unowned group: %v", groups)
	}
}
func TestValidationRejectsEscalation(t *testing.T) {
	p := examplePolicy()
	p.Personas["ai"] = Persona{Keep: []string{"research"}, Add: []string{"wheel"}, SlurmAccount: "agents"}
	if p.validate() == nil {
		t.Fatal("privileged group add accepted")
	}
	p = examplePolicy()
	p.SharedPaths = []string{"../.ssh"}
	if p.validate() == nil {
		t.Fatal("escaped path accepted")
	}
	p = examplePolicy()
	p.ProvisionURL = "http://localhost"
	if p.validate() == nil {
		t.Fatal("insecure provisioning transport accepted")
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
