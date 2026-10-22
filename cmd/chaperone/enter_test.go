package main

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
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

func TestAllowedGIDs(t *testing.T) {
	p := examplePolicy()
	human := &user.User{Username: "alice", Gid: "1000"}
	dir := map[string]string{"2001": "lab", "2003": "restricted"} // 2002: deleted
	lookup := func(g string) (*user.Group, error) {
		if n, ok := dir[g]; ok {
			return &user.Group{Gid: g, Name: n}, nil
		}
		return nil, user.UnknownGroupIdError(g)
	}
	gids := []string{"1000", "2001", "2002", "2003"}
	// A deleted group is left out, and the launch goes on.
	got, err := allowedGIDs(p, p.Personas["ai"], human, "3000", gids, lookup)
	if err != nil || !reflect.DeepEqual(got, []uint32{3000, 2001}) {
		t.Fatalf("deleted group: %v %v", got, err)
	}
	// Recreated under a banned name, it stays out.
	dir["2002"] = "restricted"
	if got, _ := allowedGIDs(p, p.Personas["ai"], human, "3000", gids, lookup); !reflect.DeepEqual(got, []uint32{3000, 2001}) {
		t.Fatalf("recreated banned group: %v", got)
	}
	// Any other lookup failure (directory unreachable) fails the launch.
	fail := func(string) (*user.Group, error) { return nil, errors.New("lookup groupid: input/output error") }
	if got, err := allowedGIDs(p, p.Personas["ai"], human, "3000", gids, fail); err == nil {
		t.Fatalf("lookup failure accepted: %v", got)
	}
}
