package main

import (
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
	"time"
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
	if _, err := p.profile(prof); err != nil {
		return err
	}
	name, err := personaName(human.Username, prof)
	if err != nil {
		return err
	}
	// Fresh identities, not SSSD's cache (90 min by default): a revoked
	// human is refused now, and the persona the broker just created resolves.
	// Clean env: sudo keeps the caller's locale and proxy variables for the
	// persona, and root's children must not see them.
	for _, u := range []string{human.Username, name} {
		c := exec.Command("sss_cache", "-u", u)
		c.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
		c.Run() // no SSSD: nothing cached
	}
	humanGroups, err := userGroups(human)
	if err != nil {
		return err
	}
	if !p.eligible(humanGroups) {
		return errors.New("human is no longer eligible")
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
	if err := checkGroups(actual, target.Username); err != nil {
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
	logLaunch(human.Username, target.Username)
	if err := syscall.Setgroups(ids); err != nil {
		return err
	}
	if err := syscall.Setgid(gid); err != nil {
		return err
	}
	if err := syscall.Setuid(uid); err != nil {
		return err
	}
	// Group rw: the human, a member of the persona's group, controls
	// whatever the persona creates, also outside shared directories.
	syscall.Umask(0o007)
	if len(command) == 0 {
		command = []string{"/bin/bash"}
	}
	drop := []string{"HOME", "USER", "LOGNAME", "PATH", "SUDO_USER", "SUDO_UID", "SUDO_GID", "SUDO_COMMAND"}
	env := slices.DeleteFunc(os.Environ(), func(v string) bool {
		k, _, _ := strings.Cut(v, "=")
		return slices.Contains(drop, k)
	})
	env = append(env, "HOME="+human.HomeDir, "USER="+target.Username, "LOGNAME="+target.Username, "PATH="+path)
	if slices.Contains(p.SharedPaths, ".claude") && os.Getenv("CLAUDE_CONFIG_DIR") == "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+filepath.Join(human.HomeDir, ".claude")) // see seedClaudeConfig
	}
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

// checkGroups requires the persona to hold its own group and nothing else
// (ipausers is FreeIPA's non-POSIX default group, shown by some SSSD setups).
func checkGroups(actual []string, persona string) error {
	if !slices.Contains(actual, persona) {
		return fmt.Errorf("missing persona group %s", persona)
	}
	for _, g := range actual {
		if g != persona && g != "ipausers" {
			return fmt.Errorf("unexpected persona group %s", g)
		}
	}
	return nil
}

// launchLog is root's record of where personas ran; _uninstall reads it.
const launchLog = "/var/log/chaperone-launches"

// logLaunch appends "time human persona cwd" (cwd quoted). Best effort: a
// launch never fails on it.
func logLaunch(human, persona string) {
	wd, _ := os.Getwd()
	wd, _ = filepath.EvalSymlinks(wd)
	f, err := os.OpenFile(launchLog, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%s\t%q\n", time.Now().Format(time.RFC3339), human, persona, wd)
}
