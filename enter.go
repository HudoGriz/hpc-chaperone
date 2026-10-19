package main

import (
	"bytes"
	"encoding/binary"
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
	expected, err := p.groups(privateGroups(human), humanGroups, prof)
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
		dir, _, err := openAt(atFDCWD, filepath.Join(human.HomeDir, rel))
		if err == nil {
			err = repairACL(dir, uid, false, true, target.Username, human.Username)
			dir.Close()
		}
		if err != nil {
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

// shareHome shares administrator-selected HOME paths with the persona:
// read-write agent state (write), or read-only per-user tool installs such as
// ~/.local/share/claude (exec_paths: the persona runs them, never changes
// them). Never traverses symlinked path components.
// This intentionally changes ACLs on the human's own files, not protected datasets.
func shareHome(human *user.User, persona string, paths []string, write bool) error {
	if write && len(paths) == 0 {
		return errors.New("no shared paths defined")
	}
	if err := traverseOnly(human.HomeDir, persona); err != nil {
		return err
	}
next:
	for _, rel := range paths {
		dir := human.HomeDir
		parts := strings.Split(rel, "/")
		for i, part := range parts {
			dir = filepath.Join(dir, part)
			info, err := os.Lstat(dir)
			if os.IsNotExist(err) {
				if !write {
					continue next // tool not installed for this human
				}
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
				if err := traverseOnly(dir, persona); err != nil {
					return err
				}
				continue
			}
			if !info.IsDir() {
				return fmt.Errorf("shared path is not directory: %s", dir)
			}
			if err := shareDir(dir, human, persona, write, write); err != nil {
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
		p := filepath.Join(home, s)
		targets := []string{p}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			targets = append(targets, r) // ~/.ssh -> ~/keys protects ~/keys too
		}
		for _, t := range targets {
			if inside(dir, t) || inside(t, dir) {
				return fmt.Errorf("%s holds credentials; not shared with the persona", dir)
			}
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
			if err := traverseOnly(p, persona); err != nil {
				return err
			}
		}
	}
	return shareDir(dir, human, persona, false, true)
}

// traverseOnly lets the persona pass through dir (--x) without reaching
// anything below it by "other" permission bits: every child with no entry for
// the persona gets an explicit u:persona:--- (a named entry beats "other"), so
// a 0644 data copy in ~/project stays unreadable. Grants made afterwards
// (shared, exec and launch directories) replace that entry; existing grants
// are never revoked. Only dir's direct children are read, so the cost per
// launch is one readdir and an xattr read per entry, not a tree walk.
func traverseOnly(path, persona string) error {
	if err := callACL("-m", "u:"+persona+":--x", path); err != nil {
		return err
	}
	pu, err := user.Lookup(persona)
	if err != nil {
		return err
	}
	puid, err := strconv.ParseUint(pu.Uid, 10, 32)
	if err != nil {
		return err
	}
	dir, _, err := openAt(atFDCWD, path)
	if err != nil {
		return err
	}
	defer dir.Close()
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}
	var block []*os.File
	flush := func() error {
		defer func() {
			for _, f := range block {
				f.Close()
			}
			block = nil
		}()
		if len(block) == 0 {
			return nil
		}
		_, err := fdACL("setfacl", []string{"-m", "u:" + persona + ":---", "--"}, block...)
		return err
	}
	for _, n := range names {
		f, st, err := openAt(int(dir.Fd()), n)
		if err != nil {
			continue // vanished, or a symlink: its target's own permissions apply
		}
		if k := st.Mode & syscall.S_IFMT; (k != syscall.S_IFDIR && k != syscall.S_IFREG) || hasUserEntry(f, uint32(puid)) {
			f.Close()
			continue
		}
		if block = append(block, f); len(block) == 128 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// hasUserEntry reports whether f's access ACL names uid (any permission).
// Format: 4-byte version, then 8-byte {tag u16, perm u16, id u32} entries,
// little endian; tag 2 is ACL_USER.
func hasUserEntry(f *os.File, uid uint32) bool {
	buf := make([]byte, 4096)
	n, err := syscall.Getxattr(fmt.Sprintf("/proc/self/fd/%d", f.Fd()), "system.posix_acl_access", buf)
	if err != nil {
		return false
	}
	for i := 4; i+8 <= n; i += 8 {
		if binary.LittleEndian.Uint16(buf[i:]) == 2 && binary.LittleEndian.Uint32(buf[i+4:]) == uid {
			return true
		}
	}
	return false
}

// shareDir gives both users rwx (write) or the persona r-x on dir and, by
// default ACL, on everything created below it. The first share also walks
// the human's existing files; repair re-walks for masked entries on every
// launch (small agent dirs only). dir is pinned by descriptor and must still
// be the human's directory at the expected path. On first share the walk
// runs before dir itself admits the persona.
func shareDir(path string, human *user.User, persona string, repair, write bool) error {
	owner, err := strconv.Atoi(human.Uid)
	if err != nil {
		return err
	}
	dir, st, err := openAt(atFDCWD, path)
	if err != nil {
		return err
	}
	defer dir.Close()
	want, _ := filepath.EvalSymlinks(path)
	got, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || int(st.Uid) != owner || got != want {
		return fmt.Errorf("%s is not a directory owned by %s", path, human.Username)
	}
	perm, users := "r-x", []string{persona}
	if write {
		perm, users = "rwx", []string{persona, human.Username}
	}
	out, _ := fdACL("getfacl", []string{"-cp", "--"}, dir)
	full := !bytes.Contains(out, []byte("default:user:"+persona+":"+perm))
	if !full && !repair {
		return nil
	}
	walk := func() {
		if err := repairACL(dir, owner, full, write, users...); err != nil {
			fmt.Fprintln(os.Stderr, "chaperone: warning: shared-state ACL repair incomplete:", err)
		}
	}
	if full {
		walk()
	}
	var top []string
	for _, u := range users {
		top = append(top, "u:"+u+":"+perm, "d:u:"+u+":"+perm)
	}
	// Fatal here: an unsupported filesystem must stop the launch.
	if _, err := fdACL("setfacl", []string{"-m", strings.Join(top, ","), "--"}, dir); err != nil {
		return err
	}
	if !full {
		walk()
	}
	return nil
}

// Linux constants absent from package syscall. O_PATH pins an inode without
// opening it for I/O.
const (
	oPath   = 0x200000
	atFDCWD = -0x64
)

// openAt opens name relative to dirfd without following a final symlink:
// a directory for reading, anything else as an O_PATH handle.
func openAt(dirfd int, name string) (*os.File, syscall.Stat_t, error) {
	var st syscall.Stat_t
	fd, err := syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOTDIR {
		fd, err = syscall.Openat(dirfd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, st, err
	}
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, st, err
	}
	return os.NewFile(uintptr(fd), name), st, nil
}

// fdACL runs getfacl/setfacl on pinned files, each named by the child's
// /proc/self/fd magic link: a pathname swapped for a symlink after the check
// cannot redirect it.
func fdACL(tool string, args []string, files ...*os.File) ([]byte, error) {
	for i := range files {
		args = append(args, fmt.Sprintf("/proc/self/fd/%d", 3+i))
	}
	cmd := exec.Command(tool, args...)
	cmd.ExtraFiles = files
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %v: %s", tool, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// repairACL gives users the owner's access class on what owner owns below
// root: rwx/rwx/rw for directories, executables and other files (write), or
// r-x/r-x/r-- (read-only), plus default entries on directories. It runs as
// owner, never as root, and every object is opened without following
// symlinks and changed through its descriptor. full=false only touches
// entries whose ACL mask a 0600/0700 create cut off (mode group bits show
// the mask), so routine launches stat but rarely write on CephFS.
// Known ceilings: the mask recalculation re-enables other masked named
// entries; files moved in with group bits and no inherited ACL are skipped
// until a full pass; multiply linked files are skipped (an ACL would reach
// every alias).
// ponytail: full walk per launch of the agent dirs; add a ctime stamp if MDS load shows it.
func repairACL(root *os.File, owner int, full, write bool, users ...string) error {
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
	// Name the permission exactly: setfacl's X would make data files executable.
	type batch struct {
		acl   string
		need  uint32 // mode group bits (the mask) when already shared
		files []*os.File
	}
	b := []batch{{acl("rwx", true), 0070, nil}, {acl("rwx", false), 0070, nil}, {acl("rw-", false), 0060, nil}}
	if !write {
		b = []batch{{acl("r-x", true), 0050, nil}, {acl("r-x", false), 0050, nil}, {acl("r--", false), 0040, nil}}
	}
	var errs []error
	flush := func(i int) {
		if len(b[i].files) > 0 {
			if _, err := fdACL("setfacl", []string{"-m", b[i].acl, "--"}, b[i].files...); err != nil {
				errs = append(errs, err)
			}
		}
		for _, f := range b[i].files {
			f.Close()
		}
		b[i].files = nil
	}
	var walk func(d *os.File)
	walk = func(d *os.File) {
		names, err := d.Readdirnames(-1)
		if err != nil {
			errs = append(errs, err)
			return
		}
		for _, n := range names {
			f, st, err := openAt(int(d.Fd()), n)
			if err != nil {
				continue // vanished, unreadable, or a symlink (ELOOP)
			}
			mode := st.Mode & 0777
			i := -1
			switch st.Mode & syscall.S_IFMT {
			case syscall.S_IFDIR:
				walk(f)
				i = 0
			case syscall.S_IFREG:
				if st.Nlink == 1 && mode&0100 != 0 {
					i = 1
				} else if st.Nlink == 1 {
					i = 2
				}
			}
			if i < 0 || int(st.Uid) != owner || (!full && mode&b[i].need == b[i].need) {
				f.Close()
				continue
			}
			if b[i].files = append(b[i].files, f); len(b[i].files) == 128 {
				flush(i)
			}
		}
	}
	walk(root)
	for i := range b {
		flush(i)
	}
	return errors.Join(errs...)
}

func callACL(args ...string) error {
	cmd := exec.Command("setfacl", args...)
	if b, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("setfacl: %v: %s", e, b)
	}
	return nil
}
