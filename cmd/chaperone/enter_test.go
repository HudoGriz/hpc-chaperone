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
		if got := launchArgv(tc.cmd, tc.login); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v login=%v: %q", tc.cmd, tc.login, got)
		}
	}
	argv := launchArgv([]string{"sh", "-c", `printf %s "$1"`, "_", "a b;$x"}, true)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b;$x" {
		t.Fatalf("login wrapper changed the arguments: %q %v", out, err)
	}
}

func TestSeedClaudeConfig(t *testing.T) {
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, ".claude"), 0700)
	seedClaudeConfig(home) // nothing to seed
	if _, err := os.Stat(filepath.Join(home, ".claude", ".claude.json")); !os.IsNotExist(err) {
		t.Fatal("seeded without a source")
	}
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark"}`), 0600)
	seedClaudeConfig(home)
	os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"light"}`), 0600)
	seedClaudeConfig(home) // once only: the persona's later changes win
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", ".claude.json")); string(b) != `{"theme":"dark"}` {
		t.Fatalf("seed: %s", b)
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
	if err := runUninstall(); err == nil || !strings.Contains(err.Error(), "root") {
		t.Fatalf("_uninstall ran without root: %v", err)
	}
}
