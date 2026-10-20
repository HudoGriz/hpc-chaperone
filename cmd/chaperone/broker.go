package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// runProvision is the broker: _enter runs it as the chaperone service user
// for one launch, with the keytab of a scoped FreeIPA role. It checks the
// human's eligibility in FreeIPA itself and creates or checks the persona.
// Anyone else running it has only their own rights.
func runProvision(args []string) error {
	// Least privilege: root is a Slurm and host superuser.
	if os.Geteuid() == 0 {
		return errors.New("_provision runs as the chaperone service user, not root")
	}
	if len(args) != 2 {
		return errors.New("usage: _provision HUMAN PROFILE")
	}
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	pr, err := p.profile(args[1])
	if err != nil {
		return err
	}
	u, err := user.Lookup(args[0])
	if err != nil || u.Uid == "0" {
		return errors.New("unknown user")
	}
	name, err := personaName(u.Username, args[1])
	if err != nil {
		return err
	}
	// One provisioning at a time per host: concurrent first launches must
	// not race to create the same persona.
	lock, err := os.OpenFile(filepath.Join(brokerHome, "lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return commandBackend{}.ensure(ctx, u.Username, u.HomeDir, name, pr.SlurmAccount, p.EligibleGroups)
}

// commandBackend runs the real ipa/sacctmgr CLIs; tests inject run.
type commandBackend struct {
	run func(context.Context, string, ...string) (string, error)
}

func cmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s failed: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return string(out), nil
}

var errIneligible = errors.New("human not in an eligible group")

// ensure checks the human's eligibility in IPA itself (fresh, unlike
// SSSD's cache), then creates or checks the persona: its own POSIX group
// (description "chaperone:<human>", the human a member, so the human
// controls the persona's files), the user (userclass "chaperone:<human>")
// with that primary group and the human's HOME, and one Slurm association.
// Anything else fails closed.
func (b commandBackend) ensure(ctx context.Context, human, home, persona, account string, eligible []string) error {
	run := b.run
	if run == nil {
		run = cmdOutput
	}
	out, err := run(ctx, "ipa", "user-show", human, "--all", "--raw")
	if err != nil && !strings.Contains(strings.ToLower(out), "not found") {
		return err
	}
	// memberOf lists nested groups too.
	if !slices.ContainsFunc(parseIPA(out)["memberof"], func(dn string) bool {
		return slices.ContainsFunc(eligible, func(g string) bool {
			return strings.HasPrefix(strings.ToLower(dn), "cn="+strings.ToLower(g)+",cn=groups,")
		})
	}) {
		return errIneligible
	}
	mark := "chaperone:" + human
	// show returns the object, creating it first if IPA has none. A name
	// owned by something else is never adopted: the marks below must match.
	show := func(kind string, add ...string) (map[string][]string, error) {
		out, err := run(ctx, "ipa", kind+"-show", persona, "--all", "--raw")
		if err != nil && strings.Contains(strings.ToLower(out), "not found") {
			if _, addErr := run(ctx, "ipa", append([]string{kind + "-add", persona}, add...)...); addErr != nil {
				// Another login node may have created it concurrently. Re-read and
				// check its ownership marker below; never adopt a foreign object.
				out, err = run(ctx, "ipa", kind+"-show", persona, "--all", "--raw")
				if err != nil {
					return nil, addErr
				}
			} else {
				out, err = run(ctx, "ipa", kind+"-show", persona, "--all", "--raw")
			}
		}
		return parseIPA(out), err
	}
	g, err := show("group", "--desc="+mark)
	if err != nil {
		return err
	}
	gid := first(g["gidnumber"])
	if first(g["description"]) != mark || gid == "" {
		return errors.New("IPA group collision: not this human's persona group")
	}
	member := func(dn string) bool { return strings.HasPrefix(strings.ToLower(dn), "uid="+strings.ToLower(human)+",") }
	if !slices.ContainsFunc(g["member"], member) {
		if _, err := run(ctx, "ipa", "group-add-member", persona, "--users="+human); err != nil {
			return err
		}
	}
	u, err := show("user", "--first=Chaperone", "--last=Persona", "--homedir="+home, "--shell=/sbin/nologin",
		// userclass, not description: with --noprivate FreeIPA writes its own
		// marker into description and fails if one is given.
		"--noprivate", "--gidnumber="+gid, "--class="+mark)
	if err != nil {
		return err
	}
	// Disabling a persona in IPA must block Chaperone launches: dropping UID
	// as root does not itself enforce IPA account-lock/PAM login checks.
	if strings.EqualFold(first(u["nsaccountlock"]), "true") {
		return errors.New("persona disabled in FreeIPA")
	}
	if first(u["homedirectory"]) != home || first(u["userclass"]) != mark || first(u["gidnumber"]) != gid {
		return errors.New("IPA persona collision or mismatched HOME/group")
	}
	// No groups beyond its own: ipausers is FreeIPA's non-POSIX default.
	for _, dn := range u["memberof"] {
		if !strings.HasPrefix(strings.ToLower(dn), "cn=ipausers,cn=groups,") {
			return fmt.Errorf("unexpected IPA membership %q", dn)
		}
	}
	out, err = run(ctx, "sacctmgr", "-n", "-P", "show", "association", "where", "user="+persona, "format=Account")
	if err != nil {
		return err
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		a := strings.TrimSpace(strings.TrimSuffix(line, "|"))
		if a == account {
			found = true
		} else if a != "" {
			return fmt.Errorf("unexpected Slurm association %q", a)
		}
	}
	if !found {
		_, err = run(ctx, "sacctmgr", "-i", "add", "user", "name="+persona, "account="+account)
		if err != nil {
			return err
		}
	}
	return nil
}

// parseIPA reads `ipa *-show --all --raw` output: lower-cased attribute to values.
func parseIPA(raw string) map[string][]string {
	m := map[string][]string{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			k = strings.ToLower(strings.TrimSpace(k))
			m[k] = append(m[k], strings.TrimSpace(v))
		}
	}
	return m
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
