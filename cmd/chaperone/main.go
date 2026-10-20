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
	"slices"
	"strings"
	"syscall"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [--] COMMAND [ARGS...]
       chaperone --list-personas
       chaperone _uninstall [--purge]  (admin only: stop personas on this host;
                                        --purge also removes what launches shared)
With no command, starts a persona shell.`)
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
	case len(os.Args) > 1 && os.Args[1] == "_share":
		err = runShare(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "_provision":
		err = runProvision(os.Args[2:])
	case len(os.Args) == 2 && os.Args[1] == "_uninstall":
		err = runUninstall(false)
	case len(os.Args) == 3 && os.Args[1] == "_uninstall" && os.Args[2] == "--purge":
		err = runUninstall(true)
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
	argv := append([]string{"sudo", "-n", exe, "_enter", "--persona", o.profile, "--path", os.Getenv("PATH"), "--"}, o.command...)
	return syscall.Exec(sudo, argv, os.Environ())
}
