package main

import (
	"context"
	"errors"
	"fmt"
	"log/syslog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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
// provision, check, then run the persona in a session of its own.
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
	if len(args) < 3 || args[0] != "--persona" {
		return errors.New("invalid _enter arguments")
	}
	prof := args[1]
	if !profileRE.MatchString(prof) {
		return errors.New("invalid arguments")
	}
	// The human's own binds (chaperone bind), then --.
	var mine []bind
	args = args[2:]
	for len(args) >= 2 && args[0] == "--bind" {
		mode, rel, _ := strings.Cut(args[1], ":")
		if (mode != "rw" && mode != "ro") || !bindable(rel) {
			return fmt.Errorf("invalid bind %q", args[1])
		}
		mine = append(mine, bind{rel, mode == "ro"})
		args = args[2:]
	}
	if len(args) == 0 || args[0] != "--" {
		return errors.New("invalid _enter arguments")
	}
	command := args[1:]
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	pr, err := p.profile(prof)
	if err != nil {
		return err
	}
	if len(mine) > 0 && !pr.UserBind {
		return fmt.Errorf("profile %s takes no binds of yours (user_bind)", prof)
	}
	argv := launchArgv(command, pr.Shell)
	if len(argv) == 0 {
		return fmt.Errorf("profile %s has no shell: give a command", prof)
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
	if err := checkPersona(target, human); err != nil {
		return err
	}
	groups, err := sessionGroups(p, pr, human, target)
	if err != nil {
		return err
	}
	home, err := filepath.EvalSymlinks(human.HomeDir)
	if err != nil {
		return err
	}
	startup := defaultStartup
	if pr.StartupFiles != nil {
		startup = *pr.StartupFiles
	}
	if err := share(human, home, target.Gid, startup); err != nil {
		return err
	}
	binds := mine
	for _, rel := range pr.Bind {
		binds = append(binds, bind{rel, false})
	}
	for _, rel := range pr.BindRO {
		binds = append(binds, bind{rel, true})
	}

	wd, _ := os.Getwd()
	logLaunch(human.Username, name, wd)

	// What sudo passes (PATH is safePath), the persona's identity, then the
	// profile's env; the login shell builds the rest from the startup files.
	drop := []string{"HOME", "USER", "LOGNAME", "SUDO_USER", "SUDO_UID", "SUDO_GID", "SUDO_COMMAND"}
	env := slices.DeleteFunc(os.Environ(), func(v string) bool {
		k, _, _ := strings.Cut(v, "=")
		return slices.Contains(drop, k)
	})
	env = append(env, "HOME="+human.HomeDir, "USER="+target.Username, "LOGNAME="+target.Username)
	path := safePath
	for k, v := range pr.Env {
		v = strings.ReplaceAll(v, "$HOME", human.HomeDir)
		env = append(env, k+"="+v) // the last of a name wins
		if k == "PATH" {
			path = v
		}
	}
	uid, _ := strconv.Atoi(target.Uid)
	gid, _ := strconv.Atoi(target.Gid)
	cmd := &exec.Cmd{Args: argv, Env: env, Dir: wd, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups},
			Pdeathsig: syscall.SIGKILL}}
	// Group rw: the human, a member of the persona's group, controls what
	// the persona creates outside HOME too.
	syscall.Umask(0o007)
	status, err := session(human, target, home, binds, cmd, path)
	if err != nil {
		return err
	}
	os.Exit(status)
	return nil
}

// launchArgv runs the command, or the shell itself, through the profile's
// login shell, which reads the human's startup files (modules, conda, PATH).
// Without a shell the command runs as given.
func launchArgv(command []string, shell string) []string {
	switch {
	case shell != "" && len(command) == 0:
		return []string{shell, "-l"}
	case shell != "":
		return append([]string{shell, "-l", "-c", `exec "$@"`, "chaperone"}, command...)
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
	c := exec.Command("/usr/sbin/sss_cache", "-u", name)
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
// the human's HOME.
func checkPersona(target, human *user.User) error {
	if target.HomeDir != human.HomeDir {
		return errors.New("persona HOME differs from human HOME")
	}
	uid, err := strconv.Atoi(target.Uid)
	if err != nil || uid == 0 || target.Uid == human.Uid {
		return errors.New("invalid persona UID")
	}
	if gid, err := strconv.Atoi(target.Gid); err != nil || gid == 0 {
		return errors.New("invalid persona GID")
	}
	// Detection only: a duplicate UID in the directory would give the
	// persona someone else's files. NSS returns one entry per ID; it must be ours.
	if u, err := user.LookupId(target.Uid); err != nil || u.Username != target.Username {
		return errors.New("persona UID is not unique")
	}
	return nil
}

// sessionGroups is the persona's own group plus the human's other groups
// the profile allows, as GIDs from NSS: the broker gave the persona the same
// in the directory, for its jobs. The human's primary group is left out: the
// directory cannot list members of a private group, and in HOME the mapping
// shows the persona's group in its place.
func sessionGroups(p Policy, pr Persona, human, persona *user.User) ([]uint32, error) {
	gids, err := human.GroupIds()
	if err != nil {
		return nil, err
	}
	lookup := func(g string) (*user.Group, error) {
		grp, err := user.LookupGroupId(g)
		if errors.As(err, new(user.UnknownGroupIdError)) {
			logf("launch human=%s: group id %s unknown to NSS (deleted), left out", human.Username, g)
		}
		return grp, err
	}
	return allowedGIDs(p, pr, human, persona.Gid, gids, lookup)
}

// allowedGIDs filters the human's GIDs by group name. A GID that NSS says
// does not exist (a group deleted from the directory, still in a cached
// membership list) is left out: no profile can name it, so it is never
// granted, and should it resolve again, its name is filtered like any
// other. Any other lookup error fails the launch: a directory outage must
// not yield a policy built from part of the human's groups.
func allowedGIDs(p Policy, pr Persona, human *user.User, own string, gids []string, lookup func(string) (*user.Group, error)) ([]uint32, error) {
	byName := map[string]uint32{}
	var names []string
	for _, g := range gids {
		if g == human.Gid {
			continue
		}
		grp, err := lookup(g)
		if errors.As(err, new(user.UnknownGroupIdError)) {
			continue
		}
		if err != nil {
			return nil, err
		}
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return nil, err
		}
		byName[grp.Name] = uint32(n)
		names = append(names, grp.Name)
	}
	o, err := strconv.ParseUint(own, 10, 32)
	if err != nil {
		return nil, err
	}
	ids := []uint32{uint32(o)}
	for _, n := range p.personaGroups(human.Username, pr, names) {
		ids = append(ids, byName[n])
	}
	return ids, nil
}

// stateDir keeps, per human and persona, what uninstall and later launches need.
const stateDir = "/var/lib/chaperone"

// defaultStartup are the startup files a login shell reads, shared with the
// persona unless the profile names others (startup_files; [] for none).
var defaultStartup = []string{".bash_profile", ".bash_login", ".profile", ".bashrc"}

// share gives the persona what it needs of the real HOME and nothing more:
// HOME's group becomes the persona's with group bits --x (traverse), each
// startup file's with r-- (its login shell reads them). The first change of
// a path records its old group and mode; uninstall restores them. Only the
// human's own HOME and regular files; anything else is left alone.
// ponytail: other bits are kept; a world-readable HOME stays listable to all but the persona.
func share(human *user.User, home, gid string, startup []string) error {
	g, _ := strconv.Atoi(gid)
	rec := filepath.Join(stateDir, "home", human.Username)
	data, _ := os.ReadFile(rec)
	set := func(rel string, dir bool, bits uint32) error {
		p := filepath.Join(home, rel)
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil || strconv.Itoa(int(st.Uid)) != human.Uid ||
			(st.Mode&syscall.S_IFMT == syscall.S_IFDIR) != dir || (!dir && st.Mode&syscall.S_IFMT != syscall.S_IFREG) {
			if dir {
				return errors.New("HOME is not a directory owned by you")
			}
			return nil
		}
		mode := st.Mode & 0o7777
		want := mode&^0o070 | bits
		if int(st.Gid) == g && mode == want {
			return nil
		}
		line := fmt.Sprintf("%d %o %s\n", st.Gid, mode, rel)
		if !strings.Contains("\n"+string(data), " "+rel+"\n") {
			if err := os.MkdirAll(filepath.Dir(rec), 0700); err != nil {
				return err
			}
			f, err := os.OpenFile(rec, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			_, err = f.WriteString(line)
			if err = errors.Join(err, f.Close()); err != nil {
				return err
			}
		}
		if err := os.Lchown(p, -1, g); err != nil {
			return err
		}
		return syscall.Chmod(p, want)
	}
	if err := set(".", true, 0o010); err != nil {
		return err
	}
	for _, rel := range startup {
		if err := set(rel, false, 0o040); err != nil {
			return err
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
