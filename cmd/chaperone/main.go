// Chaperone is a small identity-aware launcher and provisioner for Slurm HPC.
// This experimental version is not a substitute for site access-policy validation.
package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [--] COMMAND [ARGS...]
       chaperone bind [--ro] DIR | unbind DIR | bind
       chaperone --list-personas
With no command, starts a persona shell. bind maps a directory of your HOME
into the persona's sessions, as yours (bind alone lists them).`)
}

type options struct {
	profile string
	list    bool
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
		case a == "--list-personas":
			o.list = true
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
	if err == nil && len(o.command) > 0 && (o.command[0] == "bind" || o.command[0] == "unbind") {
		return runBind(o.command)
	}
	if err != nil {
		if err.Error() == "help" {
			usage()
			return nil
		}
		return err
	}
	if _, err = p.profile(o.profile); err != nil {
		return err
	}
	if o.list {
		for _, k := range slices.Sorted(maps.Keys(p.Personas)) {
			fmt.Println(k)
		}
		return nil
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
	binds, err := readBinds()
	if err != nil {
		return err
	}
	for _, b := range binds {
		argv = append(argv, "--bind", b)
	}
	argv = append(append(argv, "--"), o.command...)
	return syscall.Exec(sudo, argv, os.Environ())
}

// bindsFile lists the human's binds, one "rw DIR" or "ro DIR" per line,
// DIR relative to HOME. _enter checks them again.
func bindsFile() (string, string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return "", "", err
	}
	return home, filepath.Join(home, ".config/chaperone/binds"), nil
}

// readBinds returns the human's binds as _enter takes them: "rw:DIR".
func readBinds() ([]string, error) {
	_, f, err := bindsFile()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(f)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if mode, rel, ok := strings.Cut(l, " "); ok {
			out = append(out, mode+":"+rel)
		}
	}
	return out, err
}

// runBind: bind [--ro] DIR, unbind DIR, or bind to list. The next launch
// applies the change.
func runBind(args []string) error {
	home, f, err := bindsFile()
	if err != nil {
		return err
	}
	binds, err := readBinds()
	if err != nil {
		return err
	}
	if len(args) == 1 && args[0] == "bind" {
		for _, b := range binds {
			fmt.Println(strings.Replace(b, ":", " ~/", 1))
		}
		return nil
	}
	mode := "rw"
	if len(args) == 3 && args[0] == "bind" && args[1] == "--ro" {
		mode, args = "ro", []string{"bind", args[2]}
	}
	if len(args) != 2 {
		usage()
		return errors.New("usage")
	}
	dir, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	if args[0] == "bind" {
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			return err
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", args[1])
		}
	}
	rel, err := filepath.Rel(home, dir)
	if err != nil || !safeRel(rel) {
		return fmt.Errorf("%s is not a directory below your HOME", args[1])
	}
	binds = slices.DeleteFunc(binds, func(b string) bool { return strings.HasSuffix(b, ":"+rel) })
	if args[0] == "bind" {
		binds = append(binds, mode+":"+rel)
	}
	var b strings.Builder
	for _, x := range binds {
		b.WriteString(strings.Replace(x, ":", " ", 1) + "\n")
	}
	if err := os.MkdirAll(filepath.Dir(f), 0700); err != nil {
		return err
	}
	return os.WriteFile(f, []byte(b.String()), 0600)
}
