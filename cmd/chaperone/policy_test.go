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
			"ai":         {SlurmAccount: "agents", Shell: "/bin/bash", ListHome: true, Login: true, Write: []string{".claude", ".config/opencode"}, Read: []string{".gitconfig"}},
			"ai-special": {SlurmAccount: "agents-special", Write: []string{".claude"}},
		},
	}
}

func TestValidationRejectsEscalation(t *testing.T) {
	if err := examplePolicy().validate(); err != nil {
		t.Fatal(err)
	}
	ok := examplePolicy()
	ok.Personas["x"] = Persona{SlurmAccount: "agents-special"} // same (no) limits as ai-special
	ok.Personas["ai"] = Persona{SlurmAccount: "agents", Shell: "/bin/bash", SlurmLimits: map[string]string{"MaxJobs": "50", "GrpTRES": "cpu=64,gres/gpu=2", "MaxWall": "1-00:00:00"}}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid limits refused: %v", err)
	}
	// HOME itself, traversal, and noncanonical paths are always forbidden.
	// Credential-file decisions are explicitly owned by the site profile:
	// no hardcoded blacklist inside Chaperone.
	for _, bad := range []string{"", ".", "..", "../x", "/etc", "a\\b", "./.ssh", ".claude/", "a//b"} {
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
	allowed := examplePolicy()
	pr := allowed.Personas["ai"]
	pr.Read = []string{".ssh", ".config"}
	pr.Write = []string{".aws"}
	pr.Workdir = "required"
	allowed.Personas["ai"] = pr
	if err := allowed.validate(); err != nil {
		t.Fatalf("explicitly configured access refused: %v", err)
	}
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Default = "missing" },
		func(p *Policy) { p.Default = "Bad Name" },
		func(p *Policy) { pr := p.Personas["ai"]; pr.Shell = ""; p.Personas["ai"] = pr },
		func(p *Policy) { p.EligibleGroups = []string{"*"} },
		func(p *Policy) { p.Personas["x y"] = Persona{SlurmAccount: "a"} },
		func(p *Policy) { p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "bad account"} },
		func(p *Policy) { p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "agents", Workdir: "sometimes"} },
		// One grant per object: nested paths would override each other.
		func(p *Policy) {
			p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "a", Write: []string{".claude"}, Read: []string{".claude/settings.json"}}
		},
		func(p *Policy) { p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "a", Read: []string{".local", ".local/bin"}} },
		// Slurm limits: known keys, plain values, one set per account.
		func(p *Policy) {
			p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "agents", SlurmLimits: map[string]string{"AdminLevel": "Admin"}}
		},
		func(p *Policy) {
			p.Personas["ai"] = Persona{Shell: "/bin/bash", SlurmAccount: "agents", SlurmLimits: map[string]string{"MaxJobs": "5; rm"}}
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
	if p.Default != "ai" || !p.Personas["ai"].ListHome || p.Personas["ai"].Workdir != "auto" || p.Personas["ai-restrictive"].ListHome {
		t.Fatalf("unexpected example: %+v", p)
	}
}

func TestPersonaRuntimeConfig(t *testing.T) {
	p := examplePolicy()
	pr := p.Personas["ai"]
	pr.Shell = "/bin/sh"
	pr.ForwardEnv = []string{"https_proxy", "ANTHROPIC_API_KEY"}
	pr.Env = map[string]string{"CLAUDE_CONFIG_DIR": "${HOME}/.claude"}
	pr.SeedFiles = []SeedFile{{From: ".claude.json", To: ".claude/.claude.json"}}
	p.Personas["ai"] = pr
	if err := p.validate(); err != nil {
		t.Fatalf("explicit profile runtime configuration refused: %v", err)
	}
	for _, change := range []func(*Persona){
		func(pr *Persona) { pr.Shell = "bash" },
		func(pr *Persona) { pr.ForwardEnv = []string{"PATH"} },
		func(pr *Persona) { pr.ForwardEnv = []string{"TOKEN", "TOKEN"} },
		func(pr *Persona) { pr.Env = map[string]string{"USER": "root"} },
		func(pr *Persona) { pr.Env = map[string]string{"ANTHROPIC_API_KEY": "duplicate"} },
		func(pr *Persona) { pr.SeedFiles = []SeedFile{{From: "../secret", To: ".claude/copy"}} },
		func(pr *Persona) { pr.SeedFiles = []SeedFile{{From: ".claude.json", To: ".gitconfig"}} },
		func(pr *Persona) { pr.SeedFiles = []SeedFile{{From: ".claude.json", To: ".claude"}} },
	} {
		bad := examplePolicy()
		pr := p.Personas["ai"]
		change(&pr)
		bad.Personas["ai"] = pr
		if err := bad.validate(); err == nil {
			t.Fatalf("unsafe runtime profile accepted: %+v", pr)
		}
	}
}

func TestDefaultShellInheritanceIsLimited(t *testing.T) {
	p := examplePolicy()
	pr, err := p.profile("ai-special")
	if err != nil || pr.Shell != "/bin/bash" {
		t.Fatalf("default shell not inherited: %+v, %v", pr, err)
	}
	if pr.ListHome || pr.Login || len(pr.ForwardEnv) != 0 || len(pr.Env) != 0 || len(pr.SeedFiles) != 0 || len(pr.Read) != 0 || !reflect.DeepEqual(pr.Write, []string{".claude"}) {
		t.Fatalf("privileges inherited from default profile: %+v", pr)
	}
	p2 := examplePolicy()
	other := p2.Personas["ai-special"]
	other.Shell = "/bin/sh"
	p2.Personas["ai-special"] = other
	pr, err = p2.profile("ai-special")
	if err != nil || pr.Shell != "/bin/sh" {
		t.Fatalf("explicit shell was overridden: %+v, %v", pr, err)
	}
}
