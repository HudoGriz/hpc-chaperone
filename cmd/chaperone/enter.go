package main

import (
	"context"
	"errors"
	"fmt"
	"log/syslog"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// safePath is the PATH of every child root starts before the persona's own
// command: root-owned directories only.
const safePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runEnter is the only privileged path, reached through the sudo rule:
// provision, check, record, share, then become the persona.
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
	// Root's own tools (setfacl, sss_cache) from root-owned directories only.
	os.Setenv("PATH", safePath)
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
	pr, err := p.profile(prof)
	if err != nil {
		return err
	}
	name, err := personaName(human.Username, prof)
	if err != nil {
		return err
	}
	// Fresh eligibility and the persona itself, from the directory, on every launch.
	if err := provision(human.Username, prof); err != nil {
		return err
	}
	target, err := freshUser(name)
	if err != nil {
		return fmt.Errorf("persona missing from NSS: %w", err)
	}
	ids, err := checkPersona(target, human)
	if err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	if err := record(human, target, pr, wd); err != nil {
		return fmt.Errorf("recording grants: %w", err)
	}
	if err := asHuman(human, "_share", "--persona", prof); err != nil {
		return fmt.Errorf("sharing with %s: %w", name, err)
	}
	logLaunch(human.Username, name, wd)

	uid, _ := strconv.Atoi(target.Uid)
	gid, _ := strconv.Atoi(target.Gid)
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
	env := []string{"HOME=" + human.HomeDir, "USER=" + target.Username, "LOGNAME=" + target.Username, "PATH=" + path}
	for _, key := range pr.ForwardEnv {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for key, value := range pr.Env {
		env = append(env, key+"="+strings.ReplaceAll(value, "${HOME}", human.HomeDir))
	}
	argv := launchArgv(command, pr)
	// Resolve only AFTER dropping root privileges.
	os.Setenv("PATH", path)
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return err
	}
	// no_new_privs is per thread: set it on the thread that forks the
	// persona, and keep that thread. No setuid binary (sudo included) gains
	// privileges below the persona.
	runtime.LockOSThread()
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38 /* PR_SET_NO_NEW_PRIVS */, 1, 0, 0, 0, 0); e != 0 {
		return e
	}
	// Wait instead of exec: after the session the persona hands its own new
	// files (e.g. a refreshed 0600 credentials file) back to the human.
	cmd := &exec.Cmd{Path: binary, Args: argv, Env: env, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
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
	for _, rel := range pr.Write {
		dir, st, err := openAt(atFDCWD, filepath.Join(human.HomeDir, rel))
		if err == nil && st.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			err = repairACL(dir, uid, false, true, target.Uid, human.Uid)
		}
		if dir != nil {
			dir.Close()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "chaperone: warning: handing persona files back to", human.Username, "incomplete:", err)
		}
	}
	os.Exit(exitStatus(cmd.ProcessState))
	return nil
}

// launchArgv runs the command, or a shell, through a login shell when the
// profile asks for the human's startup files (modules, conda, PATH).
func launchArgv(command []string, pr Persona) []string {
	shell := pr.Shell
	switch {
	case pr.Login && len(command) == 0:
		return []string{shell, "-l"}
	case pr.Login:
		return append([]string{shell, "-l", "-c", `exec "$@"`, "chaperone"}, command...)
	case len(command) == 0:
		return []string{shell}
	}
	return command
}

// brokerHome holds the broker's Kerberos cache and lock; install.yml makes
// it, owned by the service user.
const brokerHome = "/var/lib/chaperone/broker"

// provision runs the broker (_provision) as the service user, never root,
// with the keytab only it can read.
func provision(human, profile string) error {
	svc, err := user.Lookup("chaperone")
	if err != nil {
		return errors.New("service user chaperone missing: is Chaperone installed?")
	}
	uid, _ := strconv.Atoi(svc.Uid)
	gid, _ := strconv.Atoi(svc.Gid)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, exe, "_provision", human, profile)
	c.Dir = "/"
	c.Env = []string{"PATH=" + safePath, "LC_ALL=C", "HOME=" + brokerHome,
		"KRB5_CLIENT_KTNAME=/etc/chaperone/chaperone.keytab", "KRB5CCNAME=FILE:" + brokerHome + "/krb5cc"}
	// Only if this node's Slurm config cannot reach slurmdbd (install.yml).
	if _, err := os.Stat("/etc/chaperone/slurm.conf"); err == nil {
		c.Env = append(c.Env, "SLURM_CONF=/etc/chaperone/slurm.conf")
	}
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}}
	out, err := c.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	logf("provisioning %s profile %s failed: %v: %s", human, profile, err, msg)
	if strings.Contains(msg, errIneligible.Error()) {
		return errors.New("you are not eligible for Chaperone")
	}
	return errors.New("provisioning failed (details in the system log)")
}

// freshUser drops SSSD's cached entry (90 min by default) so a persona the
// broker just created resolves; the negative cache can lag a few seconds.
func freshUser(name string) (*user.User, error) {
	c := exec.Command("sss_cache", "-u", name)
	c.Env = []string{"PATH=" + safePath}
	c.Run() // no SSSD: nothing cached
	var err error
	for range 5 {
		var u *user.User
		if u, err = user.Lookup(name); err == nil {
			return u, nil
		}
		time.Sleep(time.Second)
	}
	return nil, err
}

// checkPersona fails closed unless target is a separate identity sharing
// the human's HOME, holding its own group only; it returns the GIDs to
// install, read once so they are exactly the ones checked.
func checkPersona(target, human *user.User) ([]int, error) {
	if target.HomeDir != human.HomeDir {
		return nil, errors.New("persona HOME differs from human HOME")
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil || uid == 0 || target.Uid == human.Uid {
		return nil, errors.New("invalid persona UID")
	}
	gid, err := strconv.Atoi(target.Gid)
	if err != nil || gid == 0 {
		return nil, errors.New("invalid persona GID")
	}
	// Detection only: a duplicate UID or GID in the directory would give the
	// persona someone else's files. NSS returns one entry per ID; it must be ours.
	if u, err := user.LookupId(target.Uid); err != nil || u.Username != target.Username {
		return nil, errors.New("persona UID is not unique")
	}
	gids, err := target.GroupIds()
	if err != nil {
		return nil, err
	}
	ids, names := make([]int, len(gids)), make([]string, len(gids))
	for i, g := range gids {
		if ids[i], err = strconv.Atoi(g); err != nil {
			return nil, err
		}
		grp, err := user.LookupGroupId(g)
		if err != nil {
			return nil, err
		}
		names[i] = grp.Name
	}
	if err := checkGroups(names, target.Username); err != nil {
		return nil, err
	}
	return ids, nil
}

// checkGroups requires the persona to hold exactly its own group.
func checkGroups(actual []string, persona string) error {
	if !slices.Contains(actual, persona) {
		return fmt.Errorf("missing persona group %s", persona)
	}
	for _, g := range actual {
		if g != persona {
			return fmt.Errorf("unexpected persona group %s", g)
		}
	}
	return nil
}

// asHuman runs this binary as the human, with the human's groups, in the
// launch directory: sharing changes only what the human may change anyway.
func asHuman(human *user.User, args ...string) error {
	uid, _ := strconv.Atoi(human.Uid)
	gid, _ := strconv.Atoi(human.Gid)
	gids, err := human.GroupIds()
	if err != nil {
		return err
	}
	groups := make([]uint32, 0, len(gids))
	for _, g := range gids {
		n, err := strconv.Atoi(g)
		if err != nil {
			return err
		}
		groups = append(groups, uint32(n))
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, args...)
	c.Env = []string{"PATH=" + safePath, "LC_ALL=C", "HOME=" + human.HomeDir, "USER=" + human.Username, "LOGNAME=" + human.Username}
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	c.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}}
	return c.Run()
}

// runShare, as the human, gives the persona what its profile grants: HOME
// traverse (or list), the write and read paths, and the launch directory.
func runShare(args []string) error {
	if os.Geteuid() == 0 {
		return errors.New("_share runs as the human, not root")
	}
	if len(args) != 2 || args[0] != "--persona" {
		return errors.New("usage: _share --persona PROFILE")
	}
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	pr, err := p.profile(args[1])
	if err != nil {
		return err
	}
	human, err := user.Current()
	if err != nil {
		return err
	}
	name, err := personaName(human.Username, args[1])
	if err != nil {
		return err
	}
	persona, err := user.Lookup(name)
	if err != nil {
		return err
	}
	wd, _ := os.Getwd()
	return share(human, persona.Uid, pr, wd)
}

func share(human *user.User, pid string, pr Persona, wd string) error {
	perm := "--x"
	if pr.ListHome {
		perm = "r-x"
	}
	if err := grantPath(human.HomeDir, "u:"+pid+":"+perm); err != nil {
		return fmt.Errorf("HOME ACL: %w", err)
	}
	if err := shareHome(human, pid, pr.Write, true); err != nil {
		return fmt.Errorf("write paths: %w", err)
	}
	if err := shareHome(human, pid, pr.Read, false); err != nil {
		return fmt.Errorf("read paths: %w", err)
	}
	if err := seedFiles(human.HomeDir, pr.SeedFiles); err != nil {
		return fmt.Errorf("profile seed_files: %w", err)
	}
	if pr.Workdir != "" && pr.Workdir != "off" {
		if err := shareWorkdir(human, pid, wd); err != nil {
			if pr.Workdir == "auto" && errors.Is(err, errWorkdirNotEligible) {
				fmt.Fprintln(os.Stderr, "chaperone: note: working directory not shared:", err)
			} else {
				return fmt.Errorf("working directory sharing failed: %w", err)
			}
		}
	}
	return nil
}

// seedFiles initializes optional harness state on first use, as the human.
// The root-owned profile names both HOME-relative paths. Neither source nor
// destination may traverse symlinks; existing destination files are untouched.
func seedFiles(home string, seeds []SeedFile) error {
	for _, seed := range seeds {
		source := filepath.Join(home, seed.From)
		dest := filepath.Join(home, seed.To)
		resolved, err := filepath.EvalSymlinks(source)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if resolved != source {
			return fmt.Errorf("seed source %q follows a symlink", seed.From)
		}
		resolved, err = filepath.EvalSymlinks(filepath.Dir(dest))
		if err != nil {
			return err
		}
		if resolved != filepath.Dir(dest) {
			return fmt.Errorf("seed destination %q traverses a symlink", seed.To)
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0660)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			os.Remove(dest)
			return errors.Join(writeErr, closeErr)
		}
	}
	return nil
}

// exitStatus maps a signal death to the shell's 128+N convention.
func exitStatus(ps *os.ProcessState) int {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ps.ExitCode()
}

// logLaunch and logf write to the system log (authpriv); best effort. The
// grants record, not the log, is what uninstall relies on.
func logLaunch(human, persona, wd string) {
	logf("launch human=%s persona=%s cwd=%q", human, persona, wd)
}

func logf(format string, args ...any) {
	if w, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_INFO, "chaperone"); err == nil {
		w.Info(fmt.Sprintf(format, args...))
		w.Close()
	}
}
