package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCheckGroups(t *testing.T) {
	if err := checkGroups([]string{"alice-ai"}, "alice-ai"); err != nil {
		t.Fatal(err)
	}
	for _, actual := range [][]string{
		{},                       // own group missing
		{"alice-ai", "ipausers"}, // no exception: a group with a GID is a group
		{"alice-ai", "research"}, // any other group
		{"alice-ai", "bob-ai"},   // someone else's persona group
	} {
		if checkGroups(actual, "alice-ai") == nil {
			t.Errorf("accepted %v", actual)
		}
	}
}

func TestLaunchArgv(t *testing.T) {
	for _, tc := range []struct {
		cmd   []string
		login bool
		want  []string
	}{
		{nil, false, []string{"/bin/bash"}},
		{nil, true, []string{"/bin/bash", "-l"}},
		{[]string{"claude", "x y"}, false, []string{"claude", "x y"}},
		// The command stays argv: no shell string is built from it.
		{[]string{"claude", "x y"}, true, []string{"/bin/bash", "-l", "-c", `exec "$@"`, "chaperone", "claude", "x y"}},
	} {
		if got := launchArgv(tc.cmd, Persona{Login: tc.login}); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v login=%v: %q", tc.cmd, tc.login, got)
		}
	}
	argv := launchArgv([]string{"sh", "-c", `printf %s "$1"`, "_", "a b;$x"}, Persona{Login: true})
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b;$x" {
		t.Fatalf("login wrapper changed the arguments: %q %v", out, err)
	}
}

func TestProfileShell(t *testing.T) {
	got := launchArgv([]string{"printf", "%s", "x y"}, Persona{Login: true, Shell: "/bin/sh"})
	want := []string{"/bin/sh", "-l", "-c", `exec "$@"`, "chaperone", "printf", "%s", "x y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("profile shell: got %q, want %q", got, want)
	}
}

func TestSeedFiles(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".agent"), 0700); err != nil {
		t.Fatal(err)
	}
	seed := []SeedFile{{From: ".agent.json", To: ".agent/.agent.json"}}
	if err := seedFiles(home, seed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".agent", ".agent.json")); !os.IsNotExist(err) {
		t.Fatal("seeded without a source")
	}
	if err := os.WriteFile(filepath.Join(home, ".agent.json"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := seedFiles(home, seed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agent.json"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := seedFiles(home, seed); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".agent", ".agent.json")); string(b) != "old" {
		t.Fatalf("seed overwrote existing state: %s", b)
	}
	if err := os.Symlink(filepath.Join(home, ".agent.json"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if err := seedFiles(home, []SeedFile{{From: "link", To: ".agent/no-link"}}); err == nil {
		t.Fatal("followed a seed source symlink")
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
		// The broker and the human's sharing never run as root.
		if err := runProvision([]string{"alice", "ai"}); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("_provision ran as root: %v", err)
		}
		if err := runShare([]string{"--persona", "ai"}); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("_share ran as root: %v", err)
		}
		return
	}
	if err := runEnter([]string{"--persona", "ai", "--path", "/bin", "--", "id"}); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("_enter ran without root: %v", err)
	}
	if err := runUninstall(true); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("_uninstall ran without root: %v", err)
	}
}
