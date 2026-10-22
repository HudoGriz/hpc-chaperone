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
With no command, starts a persona shell. bind maps a directory of your HOME
into the persona's sessions, as yours (bind alone lists them).`)
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
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", args[1])
		}
		if err := checkTree(dir, pr.BanGroups); err != nil {
			return fmt.Errorf("bind refused: %w", err)
		}
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || !bindable(rel) {
		return fmt.Errorf("%s: not a directory below your HOME, or it holds %s", args[1], bindsRel)
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
func checkTree(root string, ban []string) error {
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
		return nil
	})
}
