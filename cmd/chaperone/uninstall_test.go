package main

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestCorruptGrantRecordFailsClosed(t *testing.T) {
	old := grantsFile
	grantsFile = filepath.Join(t.TempDir(), "grants")
	defer func() { grantsFile = old }()
	if err := os.WriteFile(grantsFile, []byte("invalid-line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readGrants(); err == nil || !strings.Contains(err.Error(), "corrupt grant record") {
		t.Fatalf("corrupt record accepted: %v", err)
	}
	before, _ := os.ReadFile(grantsFile)
	if err := record(&user.User{Username: "alice", Uid: "1001", Gid: "1001", HomeDir: t.TempDir()}, &user.User{Username: "alice-ai", Uid: "1002"}, Persona{}, ""); err == nil {
		t.Fatal("record overwrote corruption")
	}
	after, _ := os.ReadFile(grantsFile)
	if string(before) != string(after) {
		t.Fatal("corrupt record overwritten")
	}
}

// The record is written before sharing; a profile path the policy drops is
// revoked on the next launch; cleaning every grant leaves no entry behind.
func TestGrantsRecordRevokeClean(t *testing.T) {
	human, pid := aclEnv(t)
	grantsFile = filepath.Join(t.TempDir(), "grants")
	defer func() { grantsFile = "/var/lib/chaperone/grants" }()
	home, _ := filepath.EvalSymlinks(human.HomeDir)
	human.HomeDir = home
	persona := *human
	persona.Username, persona.Uid = "nobody", pid
	work := filepath.Join(home, "proj")
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	before := getfacl(t, home)
	pr := Persona{ListHome: true, Workdir: "auto", Write: []string{".claude", ".codex"}}
	if err := record(human, &persona, pr, work); err != nil {
		t.Fatal(err)
	}
	if err := share(human, pid, pr, work); err != nil {
		t.Fatal(err)
	}
	gs, err := readGrants()
	if err != nil || len(gs) != 4 {
		t.Fatalf("want HOME, two paths and the launch dir recorded: %+v %v", gs, err)
	}
	for _, g := range gs {
		if g.puid != pid || g.huid != human.Uid || !strings.HasPrefix(g.path, home) {
			t.Fatalf("grant %+v", g)
		}
	}
	// .codex leaves the policy: revoked at the next record.
	pr.Write = []string{".claude"}
	if err := record(human, &persona, pr, work); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, filepath.Join(home, ".codex")); strings.Contains(a, "nobody") || strings.Contains(a, "default:") {
		t.Fatalf("revoked path still shared:\n%s", a)
	}
	if gs, _ := readGrants(); len(gs) != 3 {
		t.Fatalf("revoked grant still recorded: %+v", gs)
	}
	// A profile that disables workdir sharing must revoke its historical
	// directory grant; not merely decline to add the next one.
	pr.Workdir = "off"
	if err := record(human, &persona, pr, work); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, work); strings.Contains(a, "nobody") || strings.Contains(a, "default:") {
		t.Fatalf("workdir grant survived policy off: %s", a)
	}
	if gs, _ := readGrants(); len(gs) != 2 {
		t.Fatalf("disabled workdir still recorded: %+v", gs)
	}
	gs, _ = readGrants()
	for _, g := range gs {
		if err := clean(g); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{".claude", "proj"} {
		if a := getfacl(t, filepath.Join(home, p)); strings.Contains(a, "nobody") || strings.Contains(a, "user:"+human.Username+":") || strings.Contains(a, "default:") {
			t.Fatalf("%s: entries left:\n%s", p, a)
		}
	}
	if after := getfacl(t, home); after != before {
		t.Fatalf("HOME ACL not restored:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// Root-only paths in the record never follow a symlink.
	if err := os.Symlink(work, filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openAbs(filepath.Join(home, "link", "x")); err == nil {
		t.Fatal("openAbs followed a symlinked component")
	}
}
