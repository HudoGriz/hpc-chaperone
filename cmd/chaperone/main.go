// Chaperone is a small identity-aware launcher and provisioner for Slurm HPC.
// This experimental version is not a substitute for site access-policy validation.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [--] COMMAND [ARGS...]
       chaperone [-p PROFILE] seed TOOL|PATH...
With no command, starts a persona shell. seed copies a tool's state (a name:
its usual HOME paths; or paths in your HOME) into the persona's HOME.`)
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
	if _, err := p.profile(o.profile); err != nil {
		return err
	}
	if len(o.command) > 0 && o.command[0] == "seed" {
		return runSeed(o.profile, o.command[1:])
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

// seedPlaces are where tools keep their state in HOME, for a tool name.
var seedPlaces = []string{".%s", ".%s.json", ".%src", ".config/%s", ".local/share/%s", ".local/state/%s"}

// runSeed copies tool state from your HOME into the persona's: tar as you,
// piped into tar as the persona, so each side reaches only what its user
// may. An argument with a / or a leading . is a path in your HOME; any other
// is a tool name, looked up in seedPlaces, asking for each path found on a
// terminal. What is named is copied, logins too, but no *.pid, *.sock or
// *.lock: they name your processes, and a tool trusting them would wait on
// your daemon instead of starting its own.
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
	pack := exec.Command("tar", append([]string{"-C", home, "--exclude=*.pid", "--exclude=*.sock", "--exclude=*.lock", "-cf", "-", "--"}, paths...)...)
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
