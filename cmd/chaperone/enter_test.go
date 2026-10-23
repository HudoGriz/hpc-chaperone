package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
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

func TestCheckTreeRefusesBannedGroup(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a/b"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a/b/f"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 { // the el9 job: the tree must not share /etc's group
		d, err := user.LookupGroup("daemon")
		if err != nil {
			t.Fatal(err)
		}
		gid, _ := strconv.Atoi(d.Gid)
		filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error { return os.Lchown(p, -1, gid) })
	}
	if err := checkTree(root, []string{"root"}, true, nil); err != nil {
		t.Fatalf("clean tree refused (symlink to /etc followed?): %v", err)
	}
	var st syscall.Stat_t
	syscall.Lstat(filepath.Join(root, "a/b/f"), &st)
	g, err := user.LookupGroupId(strconv.Itoa(int(st.Gid)))
	if err != nil {
		t.Fatal(err)
	}
	if err := checkTree(root, []string{g.Name}, true, nil); err == nil || !strings.Contains(err.Error(), "banned") {
		t.Fatalf("tree of a banned group accepted: %v", err)
	}
}

// A persona that may write an included directory may swap its entries:
// setOwn changes only the inode that was checked, never a symlink's target.
func TestSetOwnLeavesReplacedEntries(t *testing.T) {
	d := t.TempDir()
	a, b, target := filepath.Join(d, "a"), filepath.Join(d, "b"), filepath.Join(d, "secret")
	for _, f := range []string{a, b, target} {
		if err := os.WriteFile(f, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var st syscall.Stat_t
	syscall.Lstat(a, &st)
	open := func(m uint32) uint32 { return m | 0o066 }
	if err := os.Rename(b, a); err != nil {
		t.Fatal(err)
	}
	if err := setOwn(a, st.Ino, os.Getgid(), open); err == nil {
		t.Fatal("changed a replaced entry")
	}
	os.Remove(a)
	if err := os.Symlink(target, a); err != nil {
		t.Fatal(err)
	}
	syscall.Stat(a, &st)
	if err := setOwn(a, st.Ino, os.Getgid(), open); err == nil {
		t.Fatal("followed a symlink")
	}
	for _, f := range []string{a, target} {
		if fi, _ := os.Stat(f); fi.Mode().Perm() != 0600 {
			t.Fatalf("%s mode %o", f, fi.Mode().Perm())
		}
	}
}

func TestExcludeRestoresAndForgets(t *testing.T) {
	d := t.TempDir()
	f, rec := filepath.Join(d, "f"), filepath.Join(d, "includes")
	if err := os.WriteFile(f, nil, 0600); err != nil {
		t.Fatal(err)
	}
	var st syscall.Stat_t
	syscall.Lstat(f, &st)
	os.Chmod(f, 0660)
	other := "1 2 600 /elsewhere"
	lines := []string{fmt.Sprintf("%d %d 600 %s", st.Ino, st.Gid, f), other}
	if err := exclude(rec, lines, d); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0600 {
		t.Fatalf("mode not restored: %o", fi.Mode().Perm())
	}
	if data, _ := os.ReadFile(rec); string(data) != other+"\n" {
		t.Fatalf("record: %q", data)
	}
}

// makeHome needs root (chown); the el9 job runs tests as root.
func TestMakeHome(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	home := t.TempDir()
	human := &user.User{Uid: "0", Gid: "0", Username: "alice", HomeDir: home}
	persona := &user.User{Uid: "65534", Gid: "65534", Username: "alice-ai", HomeDir: personaHome(home, "alice-ai")}
	created, err := makeHome(human, persona, home, 0)
	if err != nil || !created {
		t.Fatalf("created=%v %v", created, err)
	}
	for p, want := range map[string]string{
		filepath.Join(home, ".chaperone"):         "0:0 710",
		persona.HomeDir:                           "65534:65534 2770",
		filepath.Join(persona.HomeDir, ".bashrc"): "65534:65534 640",
	} {
		var st syscall.Stat_t
		syscall.Lstat(p, &st)
		if got := fmt.Sprintf("%d:%d %o", st.Uid, st.Gid, st.Mode&0o7777); got != want {
			t.Errorf("%s: %s, want %s", p, got, want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(persona.HomeDir, ".bash_profile")); !strings.Contains(string(b), "HOME='"+home+"'") {
		t.Errorf(".bash_profile does not load the human's: %s", b)
	}
	if created, err := makeHome(human, persona, home, 0); err != nil || created {
		t.Fatalf("second launch: created=%v %v", created, err)
	}
	// A symlink planted for ~/.chaperone is refused and its target untouched.
	evil, target := t.TempDir(), t.TempDir()
	os.Chmod(target, 0o755)
	if err := os.Symlink(target, filepath.Join(evil, ".chaperone")); err != nil {
		t.Fatal(err)
	}
	if _, err := makeHome(&user.User{Uid: "0", Gid: "0", Username: "bob", HomeDir: evil}, persona, evil, 0); err == nil {
		t.Fatal("followed a planted symlink")
	}
	if entries, _ := os.ReadDir(target); len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v", entries)
	}
}
