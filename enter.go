package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

func runEnter(args []string) error {
	if os.Geteuid() != 0 {
		return errors.New("_enter must run through configured root-owned sudo rule")
	}
	suid := os.Getenv("SUDO_UID")
	sname := os.Getenv("SUDO_USER")
	if suid == "" || sname == "" || suid == "0" {
		return errors.New("no valid sudo origin")
	}
	human, err := user.LookupId(suid)
	if err != nil || human.Username != sname {
		return errors.New("invalid sudo origin")
	}
	if len(args) < 5 || args[0] != "--persona" || args[2] != "--path" || args[4] != "--" {
		return errors.New("invalid _enter arguments")
	}
	prof, path, command := args[1], args[3], args[5:]
	if !profileRE.MatchString(prof) || strings.ContainsAny(path, "\x00\n\r") {
		return errors.New("invalid arguments")
	}
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	humanGroups, err := userGroups(human)
	if err != nil {
		return err
	}
	if !p.eligible(humanGroups) {
		return errors.New("human is no longer eligible")
	}
	expected, err := p.groups(human.Username, humanGroups, prof)
	if err != nil {
		return err
	}
	name, err := personaName(human.Username, prof)
	if err != nil {
		return err
	}
	// The broker just changed IPA; drop SSSD's cached entry (up to 90 min
	// stale by default) so the checks below see current groups.
	if _, err := exec.LookPath("sss_cache"); err == nil {
		exec.Command("sss_cache", "-u", name).Run()
	}
	target, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("persona missing from NSS: %w", err)
	}
	if target.HomeDir != human.HomeDir {
		return errors.New("persona HOME differs from human HOME")
	}
	// One NSS read: the groups installed below are exactly the ones checked.
	gids, err := target.GroupIds()
	if err != nil {
		return err
	}
	ids, actual := make([]int, len(gids)), make([]string, len(gids))
	for i, g := range gids {
		if ids[i], err = strconv.Atoi(g); err != nil {
			return err
		}
		grp, err := user.LookupGroupId(g)
		if err != nil {
			return err
		}
		actual[i] = grp.Name
	}
	if err := checkGroups(actual, expected, target.Username); err != nil {
		return err
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil || uid == 0 || target.Uid == human.Uid {
		return errors.New("invalid persona UID")
	}
	gid, err := strconv.Atoi(target.Gid)
	if err != nil || gid == 0 {
		return errors.New("invalid persona GID")
	}
	if err := syscall.Setgroups(ids); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	if err := syscall.Setuid(uid); err != nil {
		return err
	}
	if len(command) == 0 {
		command = []string{"/bin/bash"}
	}
	drop := []string{"HOME", "USER", "LOGNAME", "PATH", "SUDO_USER", "SUDO_UID", "SUDO_GID", "SUDO_COMMAND"}
	env := slices.DeleteFunc(os.Environ(), func(v string) bool {
		k, _, _ := strings.Cut(v, "=")
		return slices.Contains(drop, k)
	})
	env = append(env, "HOME="+human.HomeDir, "USER="+target.Username, "LOGNAME="+target.Username, "PATH="+path)
	// Resolve only AFTER dropping root privileges.
	os.Setenv("PATH", path)
	binary, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	// Wait instead of exec: after the session the persona hands its own new
	// files (e.g. a refreshed 0600 credentials file) back to the human.
	cmd := &exec.Cmd{Path: binary, Args: command, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	sig := make(chan os.Signal, 1)
	// The terminal already sends SIGINT/SIGQUIT to the child's process group.
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() {
		for s := range sig {
			if s == syscall.SIGTERM || s == syscall.SIGHUP {
				cmd.Process.Signal(s)
			}
		}
	}()
	cmd.Wait()
	for _, rel := range p.SharedPaths {
		if err := repairACL(filepath.Join(human.HomeDir, rel), target.Username, false, target.Username, human.Username); err != nil {
			fmt.Fprintln(os.Stderr, "chaperone: warning: handing persona files back to", human.Username, "incomplete:", err)
		}
	}
	os.Exit(exitStatus(cmd.ProcessState))
	return nil
}

// exitStatus maps a signal death to the shell's 128+N convention.
func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// checkGroups requires the NSS-resolved groups to equal the policy groups.
// NSS also reports the FreeIPA user private group (named like the persona),
// and the non-POSIX ipausers may appear on some SSSD setups.
func checkGroups(actual, expected []string, persona string) error {
	for _, g := range expected {
		if !slices.Contains(actual, g) {
			return fmt.Errorf("missing persona group %s", g)
		}
	}
	for _, g := range actual {
		if g != "ipausers" && g != persona && !slices.Contains(expected, g) {
			return fmt.Errorf("unexpected persona group %s", g)
		}
	}
	return nil
}

// Share only administrator-selected paths; never traverse top-level symlinks.
// This intentionally changes ACLs on the human's own files, not protected datasets.
func shareHome(human *user.User, persona string, paths []string) error {
	if len(paths) == 0 {
		return errors.New("no shared paths defined")
	}
	if err := callACL("-m", "u:"+persona+":--x", human.HomeDir); err != nil {
		return err
	}
	for _, rel := range paths {
		dir := human.HomeDir
		parts := strings.Split(rel, "/")
		for i, part := range parts {
			dir = filepath.Join(dir, part)
			info, err := os.Lstat(dir)
			if os.IsNotExist(err) {
				// A first-time harness user may not have state directories yet.
				// Create them as the human before applying both users' ACLs.
				if err := os.Mkdir(dir, 0700); err != nil {
					return err
				}
				info, err = os.Lstat(dir)
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink in shared path %s", dir)
			}
			if i < len(parts)-1 {
				if err := callACL("-m", "u:"+persona+":--x", dir); err != nil {
					return err
				}
				continue
			}
			if !info.IsDir() {
				return fmt.Errorf("shared path is not directory: %s", dir)
			}
			if err := shareDir(dir, human.Username, persona, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// shareWorkdir lets the persona work where it was launched: any directory the
// human owns, except HOME itself and anything holding credentials. Only the
// first launch in a tree walks it; later files inherit the default ACL, so
// there is no per-launch walk of large project trees on CephFS.
// ponytail: a file a tool creates 0600 there stays masked from the other user
// until `chaperone -- chmod`; add a bounded repair pass if that bites.
func shareWorkdir(human *user.User, persona, wd string) error {
	dir, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return err
	}
	home, err := filepath.EvalSymlinks(human.HomeDir)
	if err != nil {
		return err
	}
	inside := func(a, b string) bool { return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/") }
	if inside(home, dir) {
		return fmt.Errorf("%s is not shared with the persona; run from a project directory", dir)
	}
	for _, s := range protectedPaths {
		if p := filepath.Join(home, s); inside(dir, p) || inside(p, dir) {
			return fmt.Errorf("%s holds credentials; not shared with the persona", dir)
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); !ok || strconv.FormatUint(uint64(st.Uid), 10) != human.Uid {
		return fmt.Errorf("%s is not yours; the persona has only its group access there", dir)
	}
	if inside(dir, home) {
		for p := filepath.Dir(dir); p != home; p = filepath.Dir(p) {
			if err := callACL("-m", "u:"+persona+":--x", p); err != nil {
				return err
			}
		}
	}
	return shareDir(dir, human.Username, persona, false)
}

// shareDir gives both users rwx on dir and, by default ACL, on everything
// created below it. The first share also walks the human's existing files;
// repair re-walks for masked entries on every launch (small agent dirs only).
func shareDir(dir, human, persona string, repair bool) error {
	out, _ := exec.Command("getfacl", "-cp", dir).Output()
	full := !bytes.Contains(out, []byte("default:user:"+persona+":rwx"))
	if !full && !repair {
		return nil
	}
	// Fatal here: an unsupported filesystem must stop the launch.
	if err := callACL("-m", "u:"+persona+":rwx,u:"+human+":rwx,d:u:"+persona+":rwx,d:u:"+human+":rwx", dir); err != nil {
		return err
	}
	if err := repairACL(dir, human, full, persona, human); err != nil {
		fmt.Fprintln(os.Stderr, "chaperone: warning: shared-state ACL repair incomplete:", err)
	}
	return nil
}

// repairACL gives users rwX on what owner owns below dir. It runs as owner,
// never as root: root following a persona-planted symlink would be an
// escalation. full=false only touches entries whose ACL mask a 0600/0700
// create cut off (mode group bits show the mask), so routine launches stat
// but rarely write on CephFS. Files may vanish while harnesses run.
// Known ceilings: setfacl follows a symlink swapped in after find's check
// (persona -> human only, inside the accepted shared-state risk); the mask
// recalculation re-enables other masked named entries; files moved in with
// group rw and no inherited ACL are skipped until a full pass.
// ponytail: find(1) walk per launch; add a ctime stamp if MDS load shows it.
func repairACL(dir, owner string, full bool, users ...string) error {
	acl := func(perm string, dflt bool) string {
		var e []string
		for _, u := range users {
			e = append(e, "u:"+u+":"+perm)
			if dflt {
				e = append(e, "d:u:"+u+":"+perm)
			}
		}
		return strings.Join(e, ",")
	}
	// Inherited default entries are rwx, so name the permission exactly;
	// setfacl's X would make every data file executable.
	passes := []struct {
		match     []string
		mask, acl string // mask: mode group bits when already shared
	}{
		{[]string{"-type", "d"}, "-g=rwx", acl("rwx", true)},
		{[]string{"-type", "f", "-perm", "-u=x"}, "-g=rwx", acl("rwx", false)},
		{[]string{"-type", "f", "!", "-perm", "-u=x"}, "-g=rw", acl("rw", false)},
	}
	for _, p := range passes {
		args := append([]string{dir, "-user", owner}, p.match...)
		if !full {
			args = append(args, "!", "-perm", p.mask)
		}
		args = append(args, "-exec", "setfacl", "-m", p.acl, "{}", "+")
		if out, err := exec.Command("find", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
		}
	}
	return nil
}

func callACL(args ...string) error {
	cmd := exec.Command("setfacl", args...)
	if b, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("setfacl: %v: %s", e, b)
	}
	return nil
}
