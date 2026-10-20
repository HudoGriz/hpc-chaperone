package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// runUninstall undoes on this host what launches did, for every persona in
// the launch log: stops its processes, gives its files in the shared
// directories to its human, and removes the persona's and human's ACL entries
// that sharing added. Run it as root after removing the sudo rule and
// cancelling persona jobs (deploy/slurm.sh uninstall), and before deleting
// the personas from the directory (names must still resolve).
// Directories are walked without following symlinks. Base ACL entries
// (mask, default user/group/other) are left: they hold no named user.
func runUninstall() error {
	if os.Geteuid() != 0 {
		return errors.New("_uninstall must run as root")
	}
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(launchLog)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	launches := map[[2]string][]string{} // {human, persona} -> launch dirs
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		f := strings.SplitN(line, "\t", 4)
		if len(f) != 4 {
			continue
		}
		wd, err := strconv.Unquote(f[3])
		if err != nil {
			continue
		}
		k := [2]string{f[1], f[2]}
		launches[k] = append(launches[k], wd)
	}
	var errs []error
	run := func(name string, args ...string) {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			errs = append(errs, fmt.Errorf("%s %v: %v: %s", name, args, err, out))
		}
	}
	for k, wds := range launches {
		h, persona := k[0], k[1]
		human, err := user.Lookup(h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		exec.Command("pkill", "-KILL", "-u", persona).Run() // exit 1: none running
		home := filepath.Clean(human.HomeDir)
		var trees []string
		for _, rel := range append(slices.Clone(p.SharedPaths), p.ExecPaths...) {
			trees = append(trees, filepath.Join(home, rel))
		}
		for _, wd := range wds {
			// Only what shareWorkdir accepts: owned by the human, not HOME or
			// its parents. Never walk a large shared tree someone launched in.
			if fi, err := os.Lstat(wd); err == nil && fi.IsDir() && !inside(home, wd) && owner(fi) == human.Uid {
				trees = append(trees, wd)
			}
		}
		pass := []string{home} // directories that only got u:persona:--x
		for _, t := range trees {
			for d := filepath.Dir(t); inside(d, home) && d != home; d = filepath.Dir(d) {
				pass = append(pass, d)
			}
		}
		slices.Sort(trees)
		for _, t := range slices.Compact(trees) {
			if _, err := os.Lstat(t); err != nil {
				continue
			}
			// Owner first: without the ACL entries the human could not open them.
			run("find", t, "-xdev", "-user", persona, "-exec", "chown", "-h", h+":", "{}", "+")
			run("setfacl", "-R", "-P", "-x", "u:"+persona+",d:u:"+persona+",u:"+h+",d:u:"+h, t)
		}
		slices.Sort(pass)
		for _, d := range slices.Compact(pass) {
			if _, err := os.Lstat(d); err == nil {
				run("setfacl", "-P", "-x", "u:"+persona, d)
			}
		}
	}
	return errors.Join(errs...)
}

func owner(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(uint64(st.Uid), 10)
	}
	return ""
}

// inside reports whether path a is b or below it.
func inside(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}
