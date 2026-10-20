package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

func TestShareHome(t *testing.T) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("needs an existing persona-like user")
	}
	human := &user.User{Username: current.Username, Uid: current.Uid, HomeDir: t.TempDir()}
	// A world-readable data copy in HOME (seen on the pilot host).
	data := filepath.Join(human.HomeDir, "project", "sample.fastq")
	if err := os.MkdirAll(filepath.Dir(data), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(data, []byte("ACGT"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(human.HomeDir, ".config"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(human.HomeDir, ".config", "other-tool"), 0755); err != nil {
		t.Fatal(err)
	}
	// Another user's entry, masked to r-x: sharing must not unmask it.
	if out, err := exec.Command("setfacl", "-m", "u:daemon:rwx,m::r-x", human.HomeDir).CombinedOutput(); err != nil {
		t.Skipf("no ACL support: %s", out)
	}
	if err := shareHome(human, "nobody", []string{".claude", ".config/opencode"}, true); err != nil {
		t.Fatal(err)
	}
	acl := func(rel string) string {
		out, err := exec.Command("getfacl", "-p", filepath.Join(human.HomeDir, rel)).CombinedOutput()
		if err != nil {
			t.Fatalf("getfacl %s: %v %s", rel, err, out)
		}
		return string(out)
	}
	if a := acl("."); !strings.Contains(a, "user:nobody:--x") || strings.Contains(a, "default:") || !strings.Contains(a, "mask::r-x") {
		t.Fatalf("HOME must be traverse-only, mask unchanged:\n%s", a)
	}
	if a := acl(".config"); !strings.Contains(a, "user:nobody:--x") {
		t.Fatalf("intermediate dir must be traverse-only:\n%s", a)
	}
	// HOME content is its owner's responsibility: untouched.
	for _, rel := range []string{"project", ".config/other-tool"} {
		if a := acl(rel); strings.Contains(a, "nobody") {
			t.Fatalf("%s got a persona entry:\n%s", rel, a)
		}
	}
	for _, rel := range []string{".claude", ".config/opencode"} {
		a := acl(rel)
		if !strings.Contains(a, "user:nobody:rwx") || !strings.Contains(a, "default:user:nobody:rwx") {
			t.Fatalf("%s not shared read-write:\n%s", rel, a)
		}
	}

	// A 0600 create under the default ACL masks the persona out (effective ---);
	// the next launch repairs it without a full re-walk.
	cred := filepath.Join(human.HomeDir, ".claude", ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if a := acl(".claude/.credentials.json"); !strings.Contains(a, "#effective:---") {
		t.Fatalf("expected masked entry before repair:\n%s", a)
	}
	if err := shareHome(human, "nobody", []string{".claude", ".config/opencode"}, true); err != nil {
		t.Fatal(err)
	}
	if a := acl(".claude/.credentials.json"); !strings.Contains(a, "user:nobody:rw-") || strings.Contains(a, "#effective") {
		t.Fatalf("0600 file not repaired:\n%s", a)
	}

	evil := &user.User{Username: current.Username, Uid: current.Uid, HomeDir: t.TempDir()}
	if err := os.Symlink(t.TempDir(), filepath.Join(evil.HomeDir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := shareHome(evil, "nobody", []string{".claude"}, true); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked shared path accepted: %v", err)
	}
	if shareHome(human, "nobody", nil, true) == nil {
		t.Fatal("empty shared paths accepted")
	}
}

func TestRepairNeverFollowsLinks(t *testing.T) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs a non-root user")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("needs an existing persona-like user")
	}
	home := t.TempDir()
	human := &user.User{Username: current.Username, Uid: current.Uid, HomeDir: home}
	secret := filepath.Join(home, ".netrc")
	if err := os.WriteFile(secret, []byte("password"), 0600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(home, ".claude")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(shared, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(shared, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := shareHome(human, "nobody", []string{".claude"}, true); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("getfacl", "-p", secret).CombinedOutput(); strings.Contains(string(out), "nobody") {
		t.Fatalf("ACL reached a file outside the shared dir via a link:\n%s", out)
	}
}

func TestExecPathsReadOnly(t *testing.T) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs a non-root user")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("needs an existing persona-like user")
	}
	home := t.TempDir()
	human := &user.User{Username: current.Username, Uid: current.Uid, HomeDir: home}
	v := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(v, 0700); err != nil {
		t.Fatal(err)
	}
	for name, mode := range map[string]os.FileMode{"2.1": 0755, "notes": 0600} {
		if err := os.WriteFile(filepath.Join(v, name), nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	// A tool the human never installed is skipped, not created.
	if err := shareHome(human, "nobody", []string{".local/share/claude", ".local/share/opencode"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "opencode")); !os.IsNotExist(err) {
		t.Fatal("missing exec path was created")
	}
	acl := func(path string) string {
		out, _ := exec.Command("getfacl", "-p", path).CombinedOutput()
		return string(out)
	}
	for path, want := range map[string]string{
		filepath.Join(home, ".local"): "user:nobody:--x",
		v:                             "default:user:nobody:r-x",
		filepath.Join(v, "2.1"):       "user:nobody:r-x",
		filepath.Join(v, "notes"):     "user:nobody:r--",
	} {
		if a := acl(path); !strings.Contains(a, want) || strings.Contains(a, "user:nobody:rw") {
			t.Errorf("%s: want %q, no write:\n%s", path, want, a)
		}
	}
}

func TestShareWorkdir(t *testing.T) {
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs a non-root user")
	}
	if _, err := user.Lookup("nobody"); err != nil {
		t.Skip("needs an existing persona-like user")
	}
	home := t.TempDir()
	human := &user.User{Username: current.Username, Uid: current.Uid, HomeDir: home}
	work := filepath.Join(home, "proj", "sub")
	for _, d := range []string{work, filepath.Join(home, ".config", "gh"), filepath.Join(home, "keys")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("keys", filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]string{
		home:                           "project directory",
		filepath.Dir(home):             "project directory",
		filepath.Join(home, ".config"): "credentials",
		filepath.Join(home, "keys"):    "credentials",
		"/":                            "project directory",
		"/usr":                         "not yours",
	} {
		if err := shareWorkdir(human, "nobody", dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", dir, err, want)
		}
	}
	existing := filepath.Join(work, "main.go")
	if err := os.WriteFile(existing, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := shareWorkdir(human, "nobody", work); err != nil {
		t.Fatal(err)
	}
	acl := func(path string) string {
		out, _ := exec.Command("getfacl", "-p", path).CombinedOutput()
		return string(out)
	}
	if a := acl(filepath.Join(home, "proj")); !strings.Contains(a, "user:nobody:--x") || strings.Contains(a, "default:") {
		t.Fatalf("parent must be traverse-only:\n%s", a)
	}
	if a := acl(work); !strings.Contains(a, "default:user:nobody:rwx") || !strings.Contains(a, "default:user:"+current.Username+":rwx") {
		t.Fatalf("workdir not shared both ways:\n%s", a)
	}
	if a := acl(existing); !strings.Contains(a, "user:nobody:rw-") {
		t.Fatalf("existing file not shared on first use:\n%s", a)
	}
	// New files inherit both users' entries without another walk.
	created := filepath.Join(work, "new.txt")
	if err := os.WriteFile(created, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if a := acl(created); !strings.Contains(a, "user:nobody:rwx\t#effective:rw-") || !strings.Contains(a, "user:"+current.Username+":rwx\t#effective:rw-") {
		t.Fatalf("new file did not inherit:\n%s", a)
	}
}
