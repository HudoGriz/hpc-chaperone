package main

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// aclEnv returns the current (non-root) user as a human with a fresh 0700
// HOME, and nobody's UID as the persona.
func aclEnv(t *testing.T) (*user.User, string) {
	t.Helper()
	if _, err := exec.LookPath("getfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs a non-root user")
	}
	nobody, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("needs an existing persona-like user")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("setfacl", "-m", "u:"+nobody.Uid+":--x", home).CombinedOutput(); err != nil {
		t.Skipf("no ACL support: %s", out)
	}
	exec.Command("setfacl", "-b", home).Run()
	return &user.User{Username: current.Username, Uid: current.Uid, Gid: current.Gid, HomeDir: home}, nobody.Uid
}

func getfacl(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command("getfacl", "-p", path).CombinedOutput()
	if err != nil {
		t.Fatalf("getfacl %s: %v %s", path, err, out)
	}
	return string(out)
}

func TestWorkdirIsExplicit(t *testing.T) {
	human, pid := aclEnv(t)
	project := filepath.Join(human.HomeDir, "project")
	if err := os.Mkdir(project, 0700); err != nil { t.Fatal(err) }
	if err := share(human, pid, Persona{Workdir: "off"}, project); err != nil { t.Fatal(err) }
	if a := getfacl(t, project); strings.Contains(a, "user:nobody:") { t.Fatal("workdir was shared despite off policy") }
	if err := share(human, pid, Persona{Workdir: "required"}, human.HomeDir); !errors.Is(err, errWorkdirNotEligible) { t.Fatalf("required mode did not reject HOME: %v", err) }
	if err := share(human, pid, Persona{Workdir: "auto"}, human.HomeDir); err != nil { t.Fatalf("auto should skip HOME: %v", err) }
	if err := share(human, pid, Persona{Workdir: "required"}, project); err != nil { t.Fatal(err) }
	if a := getfacl(t, project); !strings.Contains(a, "user:nobody:rwx") { t.Fatalf("required workdir not shared: %s", a) }
}

func TestGrantOnPrivateHome(t *testing.T) {
	human, pid := aclEnv(t)
	// A 0700 HOME without any ACL: the new entry must not be masked out.
	if err := grantPath(human.HomeDir, "u:"+pid+":--x"); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, human.HomeDir); !strings.Contains(a, "user:nobody:--x") || strings.Contains(a, "#effective") {
		t.Fatalf("persona masked out of a 0700 HOME:\n%s", a)
	}
	// list_home widens the persona's entry only.
	if err := grantPath(human.HomeDir, "u:"+pid+":r-x"); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, human.HomeDir); !strings.Contains(a, "user:nobody:r-x") || strings.Contains(a, "#effective") {
		t.Fatalf("listable HOME:\n%s", a)
	}
	if err := revokePath(human.HomeDir, pid); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, human.HomeDir); strings.Contains(a, "nobody") {
		t.Fatalf("revoke left the persona:\n%s", a)
	}
}

func TestShareKeepsOthersMasked(t *testing.T) {
	human, pid := aclEnv(t)
	for _, d := range []string{".config/other-tool", "project"} {
		if err := os.MkdirAll(filepath.Join(human.HomeDir, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	// Another user's entry, masked to r-x: sharing must not unmask it.
	if out, err := exec.Command("setfacl", "-m", "u:daemon:rwx,m::r-x", human.HomeDir).CombinedOutput(); err != nil {
		t.Fatalf("%s", out)
	}
	pr := Persona{Write: []string{".claude", ".config/opencode"}}
	if err := share(human, pid, pr, ""); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, human.HomeDir); !strings.Contains(a, "user:nobody:--x") || !strings.Contains(a, "user:daemon:r-x") || !strings.Contains(a, "mask::r-x") || strings.Contains(a, "default:") {
		t.Fatalf("HOME must be traverse-only, daemon still r-x:\n%s", a)
	}
	if a := getfacl(t, filepath.Join(human.HomeDir, ".config")); !strings.Contains(a, "user:nobody:--x") {
		t.Fatalf("intermediate dir must be traverse-only:\n%s", a)
	}
	for _, rel := range []string{"project", ".config/other-tool"} {
		if a := getfacl(t, filepath.Join(human.HomeDir, rel)); strings.Contains(a, "nobody") {
			t.Fatalf("%s got a persona entry:\n%s", rel, a)
		}
	}
	for _, rel := range pr.Write {
		if a := getfacl(t, filepath.Join(human.HomeDir, rel)); !strings.Contains(a, "user:nobody:rwx") || !strings.Contains(a, "default:user:nobody:rwx") {
			t.Fatalf("%s not shared read-write:\n%s", rel, a)
		}
	}
	// A 0600 create under the default ACL masks the persona out (effective ---);
	// the next launch repairs it without a full re-walk.
	cred := filepath.Join(human.HomeDir, ".claude", ".credentials.json")
	if err := os.WriteFile(cred, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, cred); !strings.Contains(a, "#effective:---") {
		t.Fatalf("expected masked entry before repair:\n%s", a)
	}
	if err := share(human, pid, pr, ""); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, cred); !strings.Contains(a, "user:nobody:rw-") || strings.Contains(a, "#effective") {
		t.Fatalf("0600 file not repaired:\n%s", a)
	}
	evil := &user.User{Username: human.Username, Uid: human.Uid, HomeDir: t.TempDir()}
	if err := os.Symlink(t.TempDir(), filepath.Join(evil.HomeDir, ".claude")); err != nil {
		t.Fatal(err)
	}
	if err := shareHome(evil, pid, []string{".claude"}, true); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked shared path accepted: %v", err)
	}
}

func TestRepairNeverFollowsLinks(t *testing.T) {
	human, pid := aclEnv(t)
	secret := filepath.Join(human.HomeDir, ".netrc")
	if err := os.WriteFile(secret, []byte("password"), 0600); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(human.HomeDir, ".claude")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(shared, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(secret, filepath.Join(shared, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := shareHome(human, pid, []string{".claude"}, true); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, secret); strings.Contains(a, "nobody") {
		t.Fatalf("ACL reached a file outside the shared dir via a link:\n%s", a)
	}
}

func TestReadPaths(t *testing.T) {
	human, pid := aclEnv(t)
	home := human.HomeDir
	v := filepath.Join(home, ".local", "share", "claude", "versions")
	if err := os.MkdirAll(v, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]os.FileMode{filepath.Join(v, "2.1"): 0755, filepath.Join(v, "notes"): 0600,
		filepath.Join(home, ".gitconfig"): 0600, filepath.Join(home, ".bash_profile"): 0644}
	for f, mode := range files {
		if err := os.WriteFile(f, nil, mode); err != nil {
			t.Fatal(err)
		}
	}
	// A tool the human never installed is skipped, not created.
	if err := shareHome(human, pid, []string{".local/share/claude", ".local/share/opencode", ".gitconfig", ".bash_profile", ".condarc"}, false); err != nil {
		t.Fatal(err)
	}
	for _, missing := range []string{".local/share/opencode", ".condarc"} {
		if _, err := os.Stat(filepath.Join(home, missing)); !os.IsNotExist(err) {
			t.Fatalf("missing read path %s was created", missing)
		}
	}
	for path, want := range map[string]string{
		filepath.Join(home, ".local"):        "user:nobody:--x",
		v:                                    "default:user:nobody:r-x",
		filepath.Join(v, "2.1"):              "user:nobody:r-x",
		filepath.Join(v, "notes"):            "user:nobody:r--",
		filepath.Join(home, ".gitconfig"):    "user:nobody:r--",
		filepath.Join(home, ".bash_profile"): "user:nobody:r--",
	} {
		if a := getfacl(t, path); !strings.Contains(a, want) || strings.Contains(a, "user:nobody:rw") || strings.Contains(a, "#effective:---") {
			t.Errorf("%s: want %q, no write, not masked:\n%s", path, want, a)
		}
	}
}

func TestShareWorkdir(t *testing.T) {
	human, pid := aclEnv(t)
	home := human.HomeDir
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
		home:               "HOME or its ancestor",
		filepath.Dir(home): "HOME or its ancestor",
		"/":                "HOME or its ancestor",
		"/usr":             "not owned by the human",
	} {
		if _, err := workdir(human, dir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", dir, err, want)
		}
	}
	// No built-in dot-directory blacklist: when explicitly selected by a
	// profile, a human-owned directory may be shared.
	if dir, err := workdir(human, filepath.Join(home, ".config", "gh")); err != nil || dir != filepath.Join(home, ".config", "gh") {
		t.Fatalf("site-approved workdir refused: %q %v", dir, err)
	}
	existing := filepath.Join(work, "main.go")
	if err := os.WriteFile(existing, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := shareWorkdir(human, pid, work); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, filepath.Join(home, "proj")); !strings.Contains(a, "user:nobody:--x") || strings.Contains(a, "default:") || strings.Contains(a, "#effective") {
		t.Fatalf("parent must be traverse-only:\n%s", a)
	}
	if a := getfacl(t, work); !strings.Contains(a, "default:user:nobody:rwx") || !strings.Contains(a, "default:user:"+human.Username+":rwx") {
		t.Fatalf("workdir not shared both ways:\n%s", a)
	}
	if a := getfacl(t, existing); !strings.Contains(a, "user:nobody:rw-") {
		t.Fatalf("existing file not shared on first use:\n%s", a)
	}
	// New files inherit both users' entries without another walk.
	created := filepath.Join(work, "new.txt")
	if err := os.WriteFile(created, nil, 0666); err != nil {
		t.Fatal(err)
	}
	if a := getfacl(t, created); !strings.Contains(a, "user:nobody:rwx\t#effective:rw-") || !strings.Contains(a, "user:"+human.Username+":rwx\t#effective:rw-") {
		t.Fatalf("new file did not inherit:\n%s", a)
	}
}

func TestWalkBudget(t *testing.T) {
	human, pid := aclEnv(t)
	work := filepath.Join(human.HomeDir, "big")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if err := os.WriteFile(filepath.Join(work, n), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	defer func(b int) { walkBudget = b }(walkBudget)
	walkBudget = 3
	if err := shareWorkdir(human, pid, work); !errors.Is(err, errTooBig) {
		t.Fatalf("budget not enforced: %v", err)
	}
	if a := getfacl(t, work); strings.Contains(a, "nobody") {
		t.Fatalf("a tree over budget still admits the persona:\n%s", a)
	}
}
