//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// This suite executes Chaperone's REAL commandBackend against disposable
// FreeIPA and Slurm services. CI places docker-exec adapters for ipa and
// sacctmgr on PATH. It never contacts an institutional identity service.
func TestLiveFreeIPAAndSlurmProvisioning(t *testing.T) {
	if os.Getenv("CHAPERONE_DISPOSABLE_INTEGRATION") != "1" {
		t.Skip("requires explicit CHAPERONE_DISPOSABLE_INTEGRATION=1 and throwaway services")
	}
	if os.Getenv("CHAPERONE_TEST_ISOLATED") != "yes" {
		t.Fatal("refusing live operations without explicit isolated test marker")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	// The broker's real rights: the IPA role from deploy/ipa.py, and Slurm
	// coordinator of the persona accounts (deploy/slurm.sh), never admin.
	backend := commandBackend{run: func(ctx context.Context, name string, args ...string) (string, error) {
		t.Logf("broker: %s %v", name, args)
		return cmdOutput(ctx, name+"-as-chaperone", args...)
	}}
	human, home := "chaperone-ci-human", "/home/chaperone-ci-human"
	// deploy/ipa.py made the human eligible and banned restricted and admins
	// for the broker; bootstrap put the human in lab and restricted.
	pol := Policy{EligibleGroups: []string{"chaperone-users"}, Personas: map[string]Persona{
		"ai":         {SlurmAccount: "agents", BanGroups: []string{"restricted", "admins"}},
		"ai-special": {SlurmAccount: "agents-special", Groups: []string{}},
	}}
	ai, special := pol.Personas["ai"], pol.Personas["ai-special"]
	defaultName, specialName := human+"-ai", human+"-ai-special"

	// Eligibility is read from IPA, not SSSD: admin is not a member.
	if err := backend.ensure(ctx, pol, ai, "admin", "/home/admin", "admin-ai"); !errors.Is(err, errIneligible) {
		t.Fatalf("ineligible human: %v", err)
	}

	// First use: the persona's own group with the human in it, the persona
	// with that primary group and the human's HOME, the human's groups
	// minus banned ones, one Slurm association.
	if err := backend.ensure(ctx, pol, ai, human, home, defaultName); err != nil {
		t.Fatalf("create persona with the broker's own rights: %v", err)
	}
	checkIPA(t, ctx, defaultName, home, human, "lab")
	checkSlurm(t, ctx, defaultName, "agents")
	if err := backend.ensure(ctx, pol, ai, human, home, defaultName); err != nil {
		t.Fatalf("idempotent provisioning: %v", err)
	}
	if err := backend.ensure(ctx, pol, special, human, home, specialName); err != nil {
		t.Fatalf("create second persona: %v", err)
	}
	checkIPA(t, ctx, specialName, home, human)
	checkSlurm(t, ctx, specialName, "agents-special")

	// FreeIPA itself keeps the broker out of banned groups.
	for _, g := range []string{"restricted", "admins"} {
		if _, err := cmdOutput(ctx, "ipa-as-chaperone", "group-add-member", g, "--users="+defaultName); err == nil {
			if out, _ := cmdOutput(ctx, "ipa", "group-show", g, "--raw"); strings.Contains(out, "uid="+defaultName+",") {
				t.Fatalf("broker added a persona to banned %s", g)
			}
		}
	}

	// The human leaves lab: the persona leaves it on the next launch.
	if _, err := cmdOutput(ctx, "ipa", "group-remove-member", "lab", "--users="+human); err != nil {
		t.Fatalf("prepare group change: %v", err)
	}
	if err := backend.ensure(ctx, pol, ai, human, home, defaultName); err != nil {
		t.Fatalf("group sync: %v", err)
	}
	checkIPA(t, ctx, defaultName, home, human)

	// An administrator's out-of-band grant of a banned group fails closed:
	// the broker may not even take it back.
	if _, err := cmdOutput(ctx, "ipa", "group-add-member", "restricted", "--users="+defaultName); err != nil {
		t.Fatalf("prepare group drift: %v", err)
	}
	if err := backend.ensure(ctx, pol, ai, human, home, defaultName); err == nil || !strings.Contains(err.Error(), "does not allow") {
		t.Fatalf("expected drift refusal, got %v", err)
	}

	// An extra Slurm association must fail closed, never be silently accepted.
	if _, err := cmdOutput(ctx, "sacctmgr", "-i", "add", "user", "name="+specialName, "account=agents"); err != nil {
		t.Fatalf("prepare foreign association: %v", err)
	}
	if err := backend.ensure(ctx, pol, special, human, home, specialName); err == nil || !strings.Contains(err.Error(), "unexpected Slurm association") {
		t.Fatalf("expected unexpected-association failure, got %v", err)
	}

	// Coordinator rights stop at the persona accounts.
	denied := human + "-outside"
	_, _ = cmdOutput(ctx, "sacctmgr-as-chaperone", "-i", "add", "user", "name="+denied, "account=root")
	if out, err := cmdOutput(ctx, "sacctmgr", "-n", "-P", "show", "association", "where", "user="+denied, "format=Account"); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("broker coordinator escaped persona accounts: %q %v", out, err)
	}

	// Existing non-Chaperone names are never adopted or modified.
	collision := human + "-collision"
	if _, err := cmdOutput(ctx, "ipa", "user-add", collision, "--first=External", "--last=Account", "--homedir=/tmp/external", "--shell=/sbin/nologin"); err != nil {
		t.Fatalf("prepare collision: %v", err)
	}
	if err := backend.ensure(ctx, pol, ai, human, home, collision); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected non-Chaperone account collision, got %v", err)
	}
	out, err := cmdOutput(ctx, "ipa", "user-show", collision, "--all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if first(parseIPA(out)["homedirectory"]) != "/tmp/external" {
		t.Fatal("external account was changed")
	}
}

// checkIPA: the persona's group and identity, and exactly groups besides ipausers.
func checkIPA(t *testing.T, ctx context.Context, persona, home, human string, groups ...string) {
	t.Helper()
	out, err := cmdOutput(ctx, "ipa", "group-show", persona, "--all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	g := parseIPA(out)
	if first(g["description"]) != "chaperone:"+human || !strings.Contains(out, "uid="+human+",") {
		t.Fatalf("persona group %s: %v", persona, g)
	}
	if out, err = cmdOutput(ctx, "ipa", "user-show", persona, "--all", "--raw"); err != nil {
		t.Fatal(err)
	}
	u := parseIPA(out)
	if first(u["homedirectory"]) != home || first(u["userclass"]) != "chaperone:"+human || first(u["gidnumber"]) != first(g["gidnumber"]) {
		t.Fatalf("persona %s: %v", persona, u)
	}
	if got := slices.DeleteFunc(groupNames(u["memberof"]), func(g string) bool { return g == "ipausers" }); !slices.Equal(slices.Sorted(slices.Values(got)), groups) {
		t.Errorf("%s groups %v, want %v", persona, got, groups)
	}
}

func checkSlurm(t *testing.T, ctx context.Context, persona, expected string) {
	t.Helper()
	out, err := cmdOutput(ctx, "sacctmgr", "-n", "-P", "show", "association", "where", "user="+persona, "format=Account")
	if err != nil {
		t.Fatalf("read Slurm associations: %v", err)
	}
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		account := strings.TrimSpace(strings.TrimSuffix(line, "|"))
		if account != "" {
			found = append(found, account)
		}
	}
	if len(found) != 1 || found[0] != expected {
		t.Fatalf("%s associations=%v, expected only %s", persona, found, expected)
	}
}
