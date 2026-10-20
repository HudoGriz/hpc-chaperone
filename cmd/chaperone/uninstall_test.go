package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBareACL(t *testing.T) {
	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("acl tools not installed")
	}
	f := filepath.Join(t.TempDir(), "f")
	os.WriteFile(f, nil, 0644)
	acl := func(args ...string) {
		if out, err := exec.Command("setfacl", append(args, f)...).CombinedOutput(); err != nil {
			t.Skipf("no ACL support here: %s", out)
		}
	}
	for _, step := range []struct {
		args []string
		want bool
	}{
		{nil, false},                          // no ACL
		{[]string{"-m", "u:nobody:r"}, false}, // named entry
		{[]string{"-x", "u:nobody"}, true},    // only the mask left
		{[]string{"-b"}, false},               // removed
	} {
		if step.args != nil {
			acl(step.args...)
		}
		if got := bare(f); got != step.want {
			t.Fatalf("after setfacl %v: bare=%v", step.args, got)
		}
	}
}
