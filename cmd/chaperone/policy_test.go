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
		Default: "ai", EligibleGroups: []string{"chaperone-users"},
		Personas: map[string]Persona{
			"ai":         {SlurmAccount: "agents", ListHome: true, Login: true, Write: []string{".claude", ".config/opencode"}, Read: []string{".gitconfig"}},
			"ai-special": {SlurmAccount: "agents-special", Write: []string{".claude"}},
		},
	}
}

func TestValidationRejectsEscalation(t *testing.T) {
	if err := examplePolicy().validate(); err != nil {
		t.Fatal(err)
	}
	// HOME itself, credentials, their ancestors, and anything not written
	// in its one canonical form.
	for _, bad := range []string{"", ".", "..", "../x", "/etc", "a\\b", "./.ssh", ".claude/", "a//b",
		".ssh", ".ssh/keys", ".config", ".config/gh", ".config/gh/hosts.yml", ".aws/x", ".netrc"} {
		for _, read := range []bool{false, true} {
			p := examplePolicy()
			pr := p.Personas["ai"]
			if read {
				pr.Read = []string{bad}
			} else {
				pr.Write = []string{bad}
			}
			p.Personas["ai"] = pr
			if p.validate() == nil {
				t.Errorf("path %q accepted (read=%v)", bad, read)
			}
		}
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Default = "missing" },
		func(p *Policy) { p.Default = "Bad Name" },
		func(p *Policy) { p.EligibleGroups = []string{"*"} },
		func(p *Policy) { p.Personas["x y"] = Persona{SlurmAccount: "a"} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "bad account"} },
		// One grant per object: nested paths would override each other.
		func(p *Policy) {
			p.Personas["ai"] = Persona{SlurmAccount: "a", Write: []string{".claude"}, Read: []string{".claude/settings.json"}}
		},
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", Read: []string{".local", ".local/bin"}} },
	} {
		p := examplePolicy()
		mutate(&p)
		if p.validate() == nil {
			t.Errorf("invalid policy accepted: %+v", p)
		}
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
		if _, err := loadPolicy("../../deploy/policy.example.json", true); err == nil || !strings.Contains(err.Error(), "root") {
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
	if err != nil || o.profile != "ai-special" || !reflect.DeepEqual(o.command, []string{"-strange", "x y"}) {
		t.Fatalf("%+v %v", o, err)
	}
	o, err = parseArgs([]string{"codex", "--model", "gpt-6"}, "ai")
	if err != nil || !reflect.DeepEqual(o.command, []string{"codex", "--model", "gpt-6"}) {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err = parseArgs([]string{"-p"}, "ai"); err == nil {
		t.Fatal("missing persona accepted")
	}
	o, err = parseArgs([]string{"--persona=ai-special", "--status", "--list-personas"}, "ai")
	if err != nil || o.profile != "ai-special" || !o.status || !o.list || o.command != nil {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err = parseArgs([]string{"--unknown"}, "ai"); err == nil {
		t.Fatal("unknown option accepted")
	}
}

func TestIncludedPolicyExample(t *testing.T) {
	p, err := loadPolicy("../../deploy/policy.example.json", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Default != "ai" || !p.Personas["ai"].ListHome || p.Personas["ai-restrictive"].ListHome {
		t.Fatalf("unexpected example: %+v", p)
	}
}
