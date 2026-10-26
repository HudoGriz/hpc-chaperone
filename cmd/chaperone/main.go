// Chaperone is a small identity-aware launcher and provisioner for Slurm HPC.
// This experimental version is not a substitute for site access-policy validation.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [--] COMMAND [ARGS...]
       chaperone [-p PROFILE] seed TOOL|PATH...
       chaperone [-p PROFILE] include [-R] PATH | exclude PATH | include
With no command, starts a persona shell. seed copies a tool's state (a name:
its usual HOME paths; or paths in your HOME) into the persona's HOME.
include gives PATH (-R: and what it holds) the persona's group, group rw;
exclude undoes it.`)
}

type options struct {
	profile string
	command []string
}

func parseArgs(args []string, fallback string) (options, error) {
	o := options{profile: fallback}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			o.command = args[i+1:]
			return o, nil
		case a == "-p" || a == "--persona":
			if i++; i >= len(args) {
				return o, errors.New("missing profile")
			}
			o.profile = args[i]
		case strings.HasPrefix(a, "--persona="):
			o.profile = strings.TrimPrefix(a, "--persona=")
		case a == "-h" || a == "--help":
			return o, errors.New("help")
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown option %s (use -- before commands beginning with '-')", a)
		default:
			o.command = args[i:]
			return o, nil
		}
	}
	return o, nil
}

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "_enter":
		err = runEnter(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "_provision":
		err = runProvision(os.Args[2:])
	default:
		if err = runClient(os.Args[1:]); err != nil {
			err = fmt.Errorf("chaperone: %w", err)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runClient checks the request and becomes sudo: _enter, as root, does
// everything else. exec, not a child: signals go to sudo directly.
func runClient(args []string) error {
	p, err := loadPolicy(sitePolicy, false)
	if err != nil {
		return err
	}
	o, err := parseArgs(args, p.Default)
	if err != nil {
		if err.Error() == "help" {
			usage()
			return nil
		}
		return err
	}
	pr, err := p.profile(o.profile)
	if err != nil {
		return err
	}
	if len(o.command) > 0 && o.command[0] == "seed" {
		return runSeed(o.profile, o.command[1:])
	}
	if len(o.command) > 0 && (o.command[0] == "include" || o.command[0] == "exclude") {
		return runInclude(pr, o.profile, o.command)
	}
	current, err := user.Current()
	if err != nil {
		return err
	}
	if current.Uid == "0" {
		return errors.New("launch Chaperone as the human user, not root")
	}
	// The sudo rule names the installed path; a copy elsewhere is refused by sudo.
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return err
	}
	argv := append([]string{"sudo", "-n", exe, "_enter", "--persona", o.profile, "--"}, o.command...)
	return syscall.Exec(sudo, argv, os.Environ())
}

// walkLimit caps the walk of an include's tree.
// ponytail: fixed; a policy knob if a site binds bigger trees.
const walkLimit = 100_000

// checkTree fails if root holds a file or directory of a banned group,
// another filesystem, or more than walkLimit entries. Symlinks are not followed.
// Without deep only root is checked; visit, if set, sees every entry.
func checkTree(root string, ban []string, deep bool, visit func(string, *syscall.Stat_t) error) error {
	banned := map[uint32]string{}
	for _, g := range ban {
		if grp, err := user.LookupGroup(g); err == nil {
			id, _ := strconv.ParseUint(grp.Gid, 10, 32)
			banned[uint32(id)] = g
		}
	}
	var dev uint64
	n := 0
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if n++; n > walkLimit {
			return fmt.Errorf("more than %d entries", walkLimit)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fi.Sys().(*syscall.Stat_t)
		if p == root {
			dev = st.Dev
		} else if st.Dev != dev {
			return fmt.Errorf("%s is another filesystem", p)
		}
		if g, ok := banned[st.Gid]; ok {
			return fmt.Errorf("%s belongs to banned group %s", p, g)
		}
		if visit != nil {
			if err := visit(p, st); err != nil {
				return err
			}
		}
		if !deep && d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})
}

// includesRel records what include changed, one "ino gid mode PATH" per
// line (PATH absolute), so that exclude restores it.
const includesRel = ".config/chaperone/includes"

// runInclude: include [-R] PATH shares PATH, which you own, with the
// persona through its group: group rw, x where the owner has it, setgid on
// directories, as a plain chgrp/chmod would; -R also everything below.
// exclude PATH restores PATH and what was recorded below it; include alone
// lists the records. Works on every node, jobs too: nothing is mounted.
func runInclude(pr Persona, profile string, args []string) error {
	me, err := user.Current()
	if err != nil {
		return err
	}
	home, err := filepath.EvalSymlinks(me.HomeDir)
	if err != nil {
		return err
	}
	rec := filepath.Join(home, includesRel)
	data, _ := os.ReadFile(rec)
	lines := strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
	lines = slices.DeleteFunc(lines, func(l string) bool { return strings.Count(l, " ") < 3 })
	if len(args) == 1 && args[0] == "include" {
		for _, l := range lines {
			fmt.Println(strings.SplitN(l, " ", 4)[3])
		}
		return nil
	}
	deep := len(args) == 3 && args[0] == "include" && args[1] == "-R"
	if deep {
		args = []string{args[0], args[2]}
	}
	if len(args) != 2 {
		return errors.New("usage: chaperone include [-R] PATH | exclude PATH | include")
	}
	p, err := filepath.Abs(args[1])
	if err == nil {
		p, err = filepath.EvalSymlinks(p)
	}
	if err != nil {
		return err
	}
	// HOME stays unwritable, the persona must not edit these records, and
	// the personas' HOMEs keep their own groups.
	conf := filepath.Join(home, filepath.Dir(includesRel))
	// The path as written and its real path: ~/.config may be a symlink.
	guard := []string{conf, filepath.Join(home, ".chaperone")}
	if c, err := filepath.EvalSymlinks(conf); err == nil {
		guard = append(guard, c)
	} else if c, err := filepath.EvalSymlinks(filepath.Dir(conf)); err == nil {
		guard = append(guard, filepath.Join(c, filepath.Base(conf)))
	}
	if strings.HasPrefix(home+"/", p+"/") || strings.Contains(p, "\n") {
		return fmt.Errorf("%s: HOME itself", args[1])
	}
	for _, d := range guard {
		if strings.HasPrefix(d+"/", p+"/") || strings.HasPrefix(p+"/", d+"/") {
			return fmt.Errorf("%s: holds or lies in %s", args[1], d)
		}
	}
	if args[0] == "exclude" {
		return exclude(rec, lines, p)
	}
	name, err := personaName(me.Username, profile)
	if err != nil {
		return err
	}
	grp, err := user.LookupGroup(name)
	if err != nil {
		return fmt.Errorf("no group %s yet: launch the persona once", name)
	}
	gid, _ := strconv.Atoi(grp.Gid)
	// chgrp needs the group in this session, not only in the directory.
	if mine, _ := os.Getgroups(); !slices.Contains(mine, gid) {
		return fmt.Errorf("this session does not hold group %s yet (the first launch made it): log in again", name)
	}
	recorded := map[string]bool{}
	for _, l := range lines {
		recorded[strings.SplitN(l, " ", 4)[3]] = true
	}
	type entry struct {
		path string
		ino  uint64
		dir  bool
	}
	var todo []entry
	var add strings.Builder
	uid := uint32(os.Getuid())
	err = checkTree(p, pr.BanGroups, deep, func(q string, st *syscall.Stat_t) error {
		t := st.Mode & syscall.S_IFMT
		if t != syscall.S_IFDIR && t != syscall.S_IFREG {
			return nil // symlinks and devices stay as they are
		}
		if st.Uid != uid {
			return fmt.Errorf("%s is not yours", q)
		}
		if strings.Contains(q, "\n") {
			return fmt.Errorf("%q: newline in name", q)
		}
		if !recorded[q] {
			fmt.Fprintf(&add, "%d %d %o %s\n", st.Ino, st.Gid, st.Mode&0o7777, q)
		}
		todo = append(todo, entry{q, st.Ino, t == syscall.S_IFDIR})
		return nil
	})
	if err != nil {
		return fmt.Errorf("include refused: %w", err)
	}
	// The record first, so that exclude can undo a run that fails halfway.
	if err := os.MkdirAll(conf, 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(rec, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(add.String())
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	var errs []error
	for _, e := range todo {
		errs = append(errs, setOwn(e.path, e.ino, gid, func(m uint32) uint32 {
			if m |= 0o060; e.dir {
				return m | 0o2010
			}
			return m | (m&0o100)>>3
		}))
	}
	return errors.Join(errs...)
}

// exclude restores the recorded group and mode of p and of what was
// recorded below it. An entry gone is dropped from the record; one that
// fails stays, for the next exclude.
func exclude(rec string, lines []string, p string) error {
	var keep []string
	var errs []error
	for _, l := range lines {
		f := strings.SplitN(l, " ", 4)
		if f[3] != p && !strings.HasPrefix(f[3], p+"/") {
			keep = append(keep, l)
			continue
		}
		ino, _ := strconv.ParseUint(f[0], 10, 64)
		gid, _ := strconv.Atoi(f[1])
		mode, _ := strconv.ParseUint(f[2], 8, 32)
		if err := setOwn(f[3], ino, gid, func(uint32) uint32 { return uint32(mode) }); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs, keep = append(errs, err), append(keep, l)
		}
	}
	return errors.Join(append(errs, os.WriteFile(rec, []byte(strings.Join(keep, "\n")+"\n"), 0600))...)
}

// setOwn sets p's group and mode through a descriptor, and only if p is
// still the inode the walk or the record saw and yours: a persona that may
// write a shared directory may swap its entries, or the paths up to them.
func setOwn(p string, ino uint64, gid int, mode func(uint32) uint32) error {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Ino != ino || st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s: replaced since it was checked, left alone", p)
	}
	if err := syscall.Fchown(fd, -1, gid); err != nil {
		return err
	}
	return syscall.Fchmod(fd, mode(st.Mode&0o7777))
}

// seedPlaces are where tools keep their state in HOME, for a tool name.
var seedPlaces = []string{".%s", ".%s.json", ".%src", ".config/%s", ".local/share/%s", ".local/state/%s"}

// runSeed copies tool state from your HOME into the persona's: tar as you,
// piped into tar as the persona, so each side reaches only what its user
// may. An argument with a / or a leading . is a path in your HOME; any other
// is a tool name, looked up in seedPlaces, asking for each path found on a
// terminal. What is named is copied, logins too.
func runSeed(profile string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: chaperone seed TOOL|PATH... (e.g. chaperone seed claude codex ~/.config/foo)")
	}
	me, err := user.Current()
	if err != nil {
		return err
	}
	home, err := filepath.EvalSymlinks(me.HomeDir)
	if err != nil {
		return err
	}
	var t syscall.Termios // a terminal, not just a character device like /dev/null
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	in := bufio.NewReader(os.Stdin)
	var paths []string
	for _, a := range args {
		if strings.Contains(a, "/") || strings.HasPrefix(a, ".") {
			abs, err := filepath.Abs(a)
			if err != nil {
				return err
			}
			dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(home, filepath.Join(dir, filepath.Base(abs)))
			if err != nil || !safeRel(rel) {
				return fmt.Errorf("%s: not in your HOME", a)
			}
			if _, err := os.Lstat(abs); err != nil {
				return err
			}
			paths = append(paths, rel)
			continue
		}
		var found []string
		for _, p := range seedPlaces {
			rel := fmt.Sprintf(p, a)
			if _, err := os.Lstat(filepath.Join(home, rel)); err == nil {
				found = append(found, rel)
			}
		}
		if len(found) == 0 {
			return fmt.Errorf("%s: none of ~/%s in your HOME; give its paths", a, strings.Join(seedPlaces, " ~/"))
		}
		for _, rel := range found {
			if e == 0 {
				fmt.Printf("Copy ~/%s? [Y/n] ", rel)
				if ans, _ := in.ReadString('\n'); strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "n") {
					continue
				}
			}
			paths = append(paths, rel)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	name, err := personaName(me.Username, profile)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	pack := exec.Command("tar", append([]string{"-C", home, "-cf", "-", "--"}, paths...)...)
	unpack := exec.Command(exe, "-p", profile, "--", "tar", "-C", personaHome(me.HomeDir, name), "-xf", "-")
	pipe, err := pack.StdoutPipe()
	if err != nil {
		return err
	}
	unpack.Dir = "/" // the persona may not enter your working directory
	unpack.Stdin, unpack.Stdout, unpack.Stderr, pack.Stderr = pipe, os.Stdout, os.Stderr, os.Stderr
	if err := pack.Start(); err != nil {
		return err
	}
	err = unpack.Start()
	pipe.Close() // the unpacking side holds it now; if it ends early, tar gets SIGPIPE, not a hang
	if err == nil {
		err = unpack.Wait()
	}
	if err = errors.Join(pack.Wait(), err); err == nil {
		fmt.Printf("copied: %s\n", strings.Join(paths, " "))
	}
	return err
}
