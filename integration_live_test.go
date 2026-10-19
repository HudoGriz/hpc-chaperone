//go:build integration

package main

import (
	"context"
	"os"
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
	backend := commandBackend{}
	human, home := "chaperone-ci-human", "/home/chaperone-ci-human"
	defaultName, specialName := human+"-ai", human+"-ai-special"

	// Initial creation: persona must be a FreeIPA user with an exact home,
	// provenance marker, retained research group and explicit agents group.
	if err := backend.ensure(ctx, human, home, defaultName, []string{"agents", "research"}, "agents"); err != nil {
		t.Fatalf("create default persona through FreeIPA and Slurm: %v", err)
	}
	checkIPA(t, ctx, defaultName, home, human, []string{"agents", "research"}, []string{"genome-project", "diagnostika"})
	checkSlurm(t, ctx, defaultName, "agents")

	// Second call must be idempotent: same account and associations.
	if err := backend.ensure(ctx, human, home, defaultName, []string{"agents", "research"}, "agents"); err != nil {
		t.Fatalf("idempotent provisioning: %v", err)
	}
	checkSlurm(t, ctx, defaultName, "agents")

	// Group revocation must propagate through the REAL IPA API, not a mock.
	if err := backend.ensure(ctx, human, home, defaultName, []string{"agents"}, "agents"); err != nil {
		t.Fatalf("revoke retained research membership: %v", err)
	}
	checkIPA(t, ctx, defaultName, home, human, []string{"agents"}, []string{"research", "genome-project", "diagnostika"})

	// A second profile is a distinct FreeIPA identity and Slurm association.
	if err := backend.ensure(ctx, human, home, specialName, []string{"agents", "research", "genome-project"}, "agents-special"); err != nil {
		t.Fatalf("create specialized persona: %v", err)
	}
	checkIPA(t, ctx, specialName, home, human, []string{"agents", "research", "genome-project"}, []string{"diagnostika"})
	checkSlurm(t, ctx, specialName, "agents-special")
	if err := backend.ensure(ctx, human, home, specialName, []string{"agents", "research", "genome-project"}, "agents-special"); err != nil {
		t.Fatalf("idempotent special provisioning: %v", err)
	}

	// Existing non-Chaperone account must never be hijacked or modified.
	collision := human + "-collision"
	_, err := cmdOutput(ctx, "ipa", "user-add", collision, "--first=External", "--last=Account", "--homedir=/tmp/external", "--shell=/sbin/nologin")
	if err != nil {
		t.Fatalf("prepare collision: %v", err)
	}
	if err := backend.ensure(ctx, human, home, collision, []string{"agents"}, "agents"); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected non-Chaperone account collision, got %v", err)
	}
	out, err := cmdOutput(ctx, "ipa", "user-show", collision, "--all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if parseIPA(out)["homedirectory"] != "/tmp/external" {
		t.Fatal("external account was changed")
	}
}

func checkIPA(t *testing.T, ctx context.Context, persona, home, human string, required, excluded []string) {
	t.Helper()
	out, err := cmdOutput(ctx, "ipa", "user-show", persona, "--all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	kv := parseIPA(out)
	if kv["homedirectory"] != home || kv["description"] != "chaperone:"+human {
		t.Fatalf("wrong FreeIPA identity metadata: %+v", kv)
	}
	found := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "memberof_group") {
			found[strings.TrimSpace(v)] = true
		}
	}
	for _, g := range required {
		if !found[g] {
			t.Errorf("%s missing group %q: %v", persona, g, found)
		}
	}
	for _, g := range excluded {
		if found[g] {
			t.Errorf("%s unexpectedly has group %q: %v", persona, g, found)
		}
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
