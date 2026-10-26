package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseIPA(t *testing.T) {
	got := parseIPA("  cn: alice-ai\n  member: uid=alice,cn=users\n  Member: uid=bob,cn=users\n  description: chaperone:alice\n")
	if !reflect.DeepEqual(got["member"], []string{"uid=alice,cn=users", "uid=bob,cn=users"}) || first(got["description"]) != "chaperone:alice" || first(got["nope"]) != "" {
		t.Fatalf("%v", got)
	}
}

// fakeSite simulates enough ipa/sacctmgr CLI behaviour to exercise the real
// commandBackend.ensure logic without FreeIPA or Slurm.
type fakeSite struct {
	users    map[string][3]string       // persona -> {homedirectory, userclass, gidnumber}
	groups   map[string][2]string       // group -> {description, gidnumber}
	members  map[string]map[string]bool // group -> member users
	memberOf map[string][]string        // human -> memberOf DNs; persona -> indirect memberOf DNs
	accounts map[string][]string        // persona -> Slurm accounts
	showErr  string                     // non-"not found" show failure
	calls    []string
}

func newFakeSite() *fakeSite {
	return &fakeSite{users: map[string][3]string{}, groups: map[string][2]string{}, members: map[string]map[string]bool{}, accounts: map[string][]string{},
		memberOf: map[string][]string{"alice": {"cn=chaperone-users,cn=groups,cn=accounts,dc=test",
			"cn=lab,cn=groups,cn=accounts,dc=test", "cn=restricted,cn=groups,cn=accounts,dc=test", "cn=alice-ai,cn=groups,cn=accounts,dc=test"}}}
}

func flag(args []string, prefix string) string {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix)
		}
	}
	return ""
}

func (f *fakeSite) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	notFound := func() (string, error) { return "ipa: ERROR: " + args[1] + ": not found", errors.New("exit status 2") }
	switch {
	case name == "ipa" && strings.HasSuffix(args[0], "-show") && f.showErr != "":
		return f.showErr, errors.New("ipa failed: exit status 1: " + f.showErr)
	case name == "ipa" && args[0] == "group-show":
		g, ok := f.groups[args[1]]
		if !ok {
			return notFound()
		}
		out := "  description: " + g[0] + "\n  gidnumber: " + g[1] + "\n"
		for m := range f.members[args[1]] {
			out += "  member: uid=" + m + ",cn=users,cn=accounts,dc=test\n"
		}
		return out, nil
	case name == "ipa" && args[0] == "group-add":
		f.groups[args[1]] = [2]string{flag(args, "--desc="), "5001"}
		return "", nil
	case name == "ipa" && args[0] == "group-add-member":
		if f.members[args[1]] == nil {
			f.members[args[1]] = map[string]bool{}
		}
		f.members[args[1]][flag(args, "--users=")] = true
		return "", nil
	case name == "ipa" && args[0] == "group-remove-member":
		if !f.members[args[1]][flag(args, "--users=")] {
			return "This entry is not a member", errors.New("exit status 1")
		}
		delete(f.members[args[1]], flag(args, "--users="))
		return "", nil
	case name == "ipa" && args[0] == "user-show":
		u, ok := f.users[args[1]]
		if !ok && f.memberOf[args[1]] != nil { // a human
			return "  memberof: " + strings.Join(f.memberOf[args[1]], "\n  memberof: ") + "\n", nil
		}
		if !ok {
			return notFound()
		}
		out := "  homedirectory: " + u[0] + "\n  userclass: " + u[1] + "\n  gidnumber: " + u[2] + "\n  memberof: cn=ipausers,cn=groups,cn=accounts,dc=test\n"
		for g, m := range f.members {
			if m[args[1]] {
				out += "  memberof: cn=" + g + ",cn=groups,cn=accounts,dc=test\n"
			}
		}
		for _, dn := range f.memberOf[args[1]] {
			out += "  memberOf: " + dn + "\n"
		}
		return out, nil
	case name == "ipa" && args[0] == "user-add":
		if !slices.Contains(args, "--noprivate") {
			return "", errors.New("persona must not get a user private group")
		}
		f.users[args[1]] = [3]string{flag(args, "--homedir="), flag(args, "--class="), flag(args, "--gidnumber=")}
		return "", nil
	case name == "sacctmgr" && slices.Contains(args, "show"):
		return strings.Join(f.accounts[flag(args, "user=")], "\n"), nil
	case name == "sacctmgr" && slices.Contains(args, "add"):
		u := flag(args, "name=")
		f.accounts[u] = append(f.accounts[u], flag(args, "account="))
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %s %v", name, args)
}

// mutations lists every state-changing call, so fail-closed tests can assert none happened.
func (f *fakeSite) mutations() []string {
	var out []string
	for _, c := range f.calls {
		if strings.Contains(c, "-add") || strings.Contains(c, " add ") {
			out = append(out, c)
		}
	}
	return out
}

var pol = Policy{EligibleGroups: []string{"chaperone-users"},
	Personas: map[string]Persona{"ai": {SlurmAccount: "agents", BanGroups: []string{"restricted"}}}}

func TestEnsureFailsClosed(t *testing.T) {
	ours := func(f *fakeSite) {
		f.groups["alice-ai"] = [2]string{"chaperone:alice", "5001"}
		f.members["alice-ai"] = map[string]bool{"alice": true}
		f.users["alice-ai"] = [3]string{"/home/alice/.chaperone/alice-ai", "chaperone:alice", "5001"}
		f.accounts["alice-ai"] = []string{"agents"}
		f.groups["alice-chaperone"] = [2]string{"chaperone:alice", "5002"}
	}
	for name, tc := range map[string]struct {
		setup func(*fakeSite)
		want  string
	}{
		"foreign group": {func(f *fakeSite) {
			f.groups["alice-ai"] = [2]string{"lab group", "5001"}
		}, "collision"},
		"foreign account": {func(f *fakeSite) {
			ours(f)
			f.users["alice-ai"] = [3]string{"/tmp/external", "", "5001"}
		}, "collision"},
		"other human's persona": {func(f *fakeSite) {
			ours(f)
			f.users["alice-ai"] = [3]string{"/home/alice/.chaperone/alice-ai", "chaperone:mallory", "5001"}
		}, "collision"},
		"other primary group": {func(f *fakeSite) {
			ours(f)
			f.users["alice-ai"] = [3]string{"/home/alice/.chaperone/alice-ai", "chaperone:alice", "100"}
		}, "collision"},
		"foreign traverse group": {func(f *fakeSite) {
			ours(f)
			f.groups["alice-chaperone"] = [2]string{"lab group", "5002"}
		}, "traverse group"},
		"foreign traverse group, first launch": {func(f *fakeSite) {
			f.groups["alice-chaperone"] = [2]string{"lab group", "5002"}
		}, "traverse group"},
		"banned group through nesting": {func(f *fakeSite) {
			ours(f)
			f.memberOf["alice-ai"] = []string{"cn=restricted,cn=groups,cn=accounts,dc=test"}
		}, "does not allow"},
		"ineligible human": {func(f *fakeSite) {
			f.memberOf["alice"] = []string{"cn=research,cn=groups,cn=accounts,dc=test"}
		}, "eligible"},
		"unknown human": {func(f *fakeSite) {
			delete(f.memberOf, "alice")
		}, "eligible"},
		"IPA unavailable": {func(f *fakeSite) {
			f.showErr = "ipa: ERROR: did not receive Kerberos credentials"
		}, "Kerberos"},
		"unexpected Slurm association": {func(f *fakeSite) {
			ours(f)
			f.accounts["alice-ai"] = []string{"agents-special"}
		}, "unexpected Slurm association"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSite()
			tc.setup(f)
			err := (commandBackend{run: f.run}).ensure(context.Background(), pol, pol.Personas["ai"], "alice", "/home/alice", "alice-ai")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Fatalf("mutated state before failing: %v", m)
			}
		})
	}
}
