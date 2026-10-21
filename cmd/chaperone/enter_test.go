package main

import (
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestLaunchArgv(t *testing.T) {
	for _, tc := range []struct {
		cmd   []string
		shell string
		want  []string
	}{
		{nil, "", nil}, // no shell, no command: refused by _enter
		{nil, "/bin/zsh", []string{"/bin/zsh", "-l"}},
		{[]string{"claude", "x y"}, "", []string{"claude", "x y"}},
		// The command stays argv: no shell string is built from it.
		{[]string{"claude", "x y"}, "/bin/zsh", []string{"/bin/zsh", "-l", "-c", `exec "$@"`, "chaperone", "claude", "x y"}},
	} {
		if got := launchArgv(tc.cmd, tc.shell); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%v shell=%q: %q", tc.cmd, tc.shell, got)
		}
	}
	argv := launchArgv([]string{"sh", "-c", `printf %s "$1"`, "_", "a b;$x"}, "/bin/sh")
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil || string(out) != "a b;$x" {
		t.Fatalf("login wrapper changed the arguments: %q %v", out, err)
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
		// The broker never runs as root.
		if err := runProvision([]string{"alice", "ai"}); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Fatalf("_provision ran as root: %v", err)
		}
		return
	}
	if err := runEnter([]string{"--persona", "ai", "--", "id"}); err == nil || !strings.Contains(err.Error(), "sudo") {
		t.Fatalf("_enter ran without root: %v", err)
	}
}
