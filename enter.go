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
	expected, err := p.groups(humanGroups, prof)
	if err != nil {
		return err
	}
	name, err := personaName(human.Username, prof)
	if err != nil {
		return err
	}
	target, err := user.Lookup(name)
	if err != nil {
		return fmt.Errorf("persona missing from NSS: %w", err)
	}
	if target.HomeDir != human.HomeDir {
		return errors.New("persona HOME differs from human HOME")
	}
	gids, err := target.GroupIds()
	if err != nil {
		return err
	}
	ids := make([]int, 0, len(gids))
	actual := make([]string, 0, len(gids))
	for _, g := range gids {
		n, e := strconv.Atoi(g)
		if e != nil {
			return e
		}
		ids = append(ids, n)
		info, e := user.LookupGroupId(g)
		if e != nil {
			return e
		}
		actual = append(actual, info.Name)
	}
	for _, g := range expected {
		if !slices.Contains(actual, g) {
			return fmt.Errorf("missing persona group %s", g)
		}
	}
	for _, g := range actual {
		if g != "ipausers" && !slices.Contains(expected, g) {
			return fmt.Errorf("unexpected persona group %s", g)
		}
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil || uid == 0 {
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
	env := os.Environ()
	env = withoutEnv(env, "HOME", "USER", "LOGNAME", "PATH", "SUDO_USER", "SUDO_UID", "SUDO_GID", "SUDO_COMMAND")
	env = append(env, "HOME="+human.HomeDir, "USER="+target.Username, "LOGNAME="+target.Username, "PATH="+path)
	// Resolve only AFTER dropping root privileges.
	os.Setenv("PATH", path)
	binary, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	return syscall.Exec(binary, command, env)
}
func withoutEnv(in []string, names ...string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		key, _, _ := strings.Cut(v, "=")
		if !slices.Contains(names, key) {
			out = append(out, v)
		}
	}
	return out
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
			if err := callACL("-R", "-m", "u:"+persona+":rwX,u:"+human.Username+":rwX", dir); err != nil {
				return err
			}
			find := exec.Command("find", dir, "-type", "d", "-exec", "setfacl", "-m", "d:u:"+persona+":rwx,d:u:"+human.Username+":rwx", "{}", "+")
			if b, e := find.CombinedOutput(); e != nil {
				return fmt.Errorf("default ACL: %v: %s", e, b)
			}
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
