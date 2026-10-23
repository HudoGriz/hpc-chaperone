// Chaperone is a small identity-aware launcher and provisioner for Slurm HPC.
// This experimental version is not a substitute for site access-policy validation.
package main

import (
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
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [--] COMMAND [ARGS...]
       chaperone bind [--ro] DIR | unbind DIR | bind
       chaperone include [-R] PATH | exclude PATH | include
With no command, starts a persona shell. bind maps a directory of your HOME
into the persona's sessions, as yours (bind alone lists them). include gives
PATH (-R: and what it holds) the persona's group, group rw; exclude undoes it.`)
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
	if len(o.command) > 0 && (o.command[0] == "bind" || o.command[0] == "unbind") {
		return runBind(pr, o.command)
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
	argv := []string{"sudo", "-n", exe, "_enter", "--persona", o.profile}
	for _, b := range readBinds(current.HomeDir) {
		argv = append(argv, "--bind", b)
	}
	argv = append(append(argv, "--"), o.command...)
	return syscall.Exec(sudo, argv, os.Environ())
}

// bindsRel holds the human's binds as _enter takes them, one "rw:DIR" or
// "ro:DIR" per line, DIR relative to HOME.
const bindsRel = ".config/chaperone/binds"

// bindable: below HOME, and not over the binds file, or the persona would
// choose the next launch's binds.
func bindable(rel string) bool {
	return safeRel(rel) && !strings.HasPrefix(bindsRel+"/", rel+"/")
}

func readBinds(home string) []string {
	data, _ := os.ReadFile(filepath.Join(home, bindsRel))
	return strings.FieldsFunc(string(data), func(r rune) bool { return r == '\n' })
}

// runBind: bind [--ro] DIR, unbind DIR, or bind alone to list them. A new
// bind's tree must hold no banned group. The next launch applies it.
func runBind(pr Persona, args []string) error {
	home, err := os.UserHomeDir()
	if err == nil {
		home, err = filepath.EvalSymlinks(home)
	}
	if err != nil {
		return err
	}
	binds := readBinds(home)
	if len(args) == 1 && args[0] == "bind" {
		fmt.Println(strings.Join(binds, "\n"))
		return nil
	}
	mode := "rw"
	if len(args) == 3 && args[1] == "--ro" {
		mode, args = "ro", []string{args[0], args[2]}
	}
	if len(args) != 2 {
		return errors.New("usage: chaperone bind [--ro] DIR | unbind DIR | bind")
	}
	dir, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	if args[0] == "bind" {
		if !pr.UserBind {
			return errors.New("your profile takes no binds of yours")
		}
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			return err
		}
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || !bindable(rel) {
		return fmt.Errorf("%s: not a directory below your HOME, or it holds %s", args[1], bindsRel)
	}
	if args[0] == "bind" {
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", args[1])
		}
		if err := checkTree(dir, pr.BanGroups, true, nil); err != nil {
			return fmt.Errorf("bind refused: %w", err)
		}
	}
	binds = slices.DeleteFunc(binds, func(b string) bool { return b[3:] == rel })
	if args[0] == "bind" {
		binds = append(binds, mode+":"+rel)
	}
	f := filepath.Join(home, bindsRel)
	if err := os.MkdirAll(filepath.Dir(f), 0700); err != nil {
		return err
	}
	return os.WriteFile(f, []byte(strings.Join(binds, "\n")+"\n"), 0600)
}

// walkLimit caps the walk of a new bind's tree.
// ponytail: fixed; a policy knob if a site binds bigger trees.
const walkLimit = 100_000

// checkTree fails if root holds a file or directory of a banned group (in a
// bind the persona is the owner, and owner bits ignore the group), another
// filesystem, or more than walkLimit entries. Symlinks are not followed.
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
	// HOME stays unwritable, and the persona must not edit binds or records.
	conf := filepath.Join(home, filepath.Dir(includesRel))
	if strings.HasPrefix(home+"/", p+"/") || strings.HasPrefix(conf+"/", p+"/") ||
		strings.HasPrefix(p+"/", conf+"/") || strings.Contains(p, "\n") {
		return fmt.Errorf("%s: HOME itself, or holds or lies in %s", args[1], conf)
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
