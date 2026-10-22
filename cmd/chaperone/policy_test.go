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
			"ai": {SlurmAccount: "agents", Shell: "/bin/bash", BanGroups: []string{"restricted"}},
			"ai-special": {SlurmAccount: "agents-special", Groups: []string{"lab"}, HomeReadOnly: true,
				Writable: []string{".claude", ".config/opencode"}, Hidden: []string{".ssh", ".netrc"}},
		},
	}
}

func TestValidationRejectsEscalation(t *testing.T) {
	if err := examplePolicy().validate(); err != nil {
		t.Fatal(err)
	}
	ok := examplePolicy()
	ok.Personas["x"] = Persona{SlurmAccount: "agents-special"} // same (no) limits as ai-special
	ok.Personas["ai"] = Persona{SlurmAccount: "agents",
		SlurmLimits: map[string]string{"MaxJobs": "50", "GrpTRES": "cpu=64,gres/gpu=2", "MaxWall": "1-00:00:00"}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid profile refused: %v", err)
	}
	// HOME itself, and anything not written in its one canonical form.
	for _, bad := range []string{"", ".", "..", "../x", "/etc", "a\\b", "./.ssh", ".claude/", "a//b"} {
		for _, hidden := range []bool{false, true} {
			p := examplePolicy()
			pr := p.Personas["ai-special"]
			if hidden {
				pr.Hidden = []string{bad}
			} else {
				pr.Writable = []string{bad}
			}
			p.Personas["ai-special"] = pr
			if p.validate() == nil {
				t.Errorf("path %q accepted (hidden=%v)", bad, hidden)
			}
		}
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Default = "missing" },
		func(p *Policy) { p.Default = "Bad Name" },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", Writable: []string{".claude"}} }, // needs home_readonly
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", BanGroups: []string{"bad group"}} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", Groups: []string{"*"}} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", Shell: "bash"} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "a", Env: map[string]string{"A=B": "x"}} },
		func(p *Policy) { p.EligibleGroups = []string{"*"} },
		func(p *Policy) { p.Personas["x y"] = Persona{SlurmAccount: "a"} },
		func(p *Policy) { p.Personas["ai"] = Persona{SlurmAccount: "bad account"} },
		// Slurm limits: one Key=Value word each, one set per account.
		func(p *Policy) {
			p.Personas["ai"] = Persona{SlurmAccount: "agents", SlurmLimits: map[string]string{"Max Jobs": "5"}}
		},
		func(p *Policy) {
			p.Personas["ai"] = Persona{SlurmAccount: "agents", SlurmLimits: map[string]string{"MaxJobs": "5; rm"}}
		},
		func(p *Policy) {
			p.Personas["x"] = Persona{SlurmAccount: "agents-special", SlurmLimits: map[string]string{"MaxJobs": "5"}}
		},
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
	o, err = parseArgs([]string{"--persona=ai-special", "--list-personas"}, "ai")
	if err != nil || o.profile != "ai-special" || !o.list || o.command != nil {
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
	if p.Default != "ai" || p.Personas["ai"].HomeReadOnly || !p.Personas["ai-restrictive"].HomeReadOnly {
		t.Fatalf("unexpected example: %+v", p)
	}
}

func TestPersonaGroups(t *testing.T) {
	p := examplePolicy()
	human := []string{"lab", "restricted", "chaperone-users", "alice-ai", "alice-ai-special", "alice-lab-x"}
	// Ban list: everything but banned, eligibility and persona groups.
	if got := p.personaGroups("alice", p.Personas["ai"], human); !reflect.DeepEqual(got, []string{"lab"}) {
		t.Fatalf("ban list: %v", got)
	}
	// Allow list: only those the human holds.
	pr := p.Personas["ai-special"]
	pr.Groups = []string{"lab", "restricted", "not-the-humans"}
	pr.BanGroups = []string{"restricted"}
	if got := p.personaGroups("alice", pr, human); !reflect.DeepEqual(got, []string{"lab"}) {
		t.Fatalf("allow list: %v", got)
	}
}
