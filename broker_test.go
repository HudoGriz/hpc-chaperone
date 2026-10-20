package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestProvisionEndpointRejectsInvalidProof(t *testing.T) {
	b := &broker{policy: examplePolicy(), verify: func(context.Context, string) (identityProof, error) { return identityProof{}, errors.New("bad") }, backend: commandBackend{run: newFakeSite().run}}
	r := httptest.NewRecorder()
	b.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/provision", strings.NewReader(`{"credential":"bogus"}`)))
	if r.Code != 401 {
		t.Fatalf("got %d: %s", r.Code, r.Body.String())
	}
	r = httptest.NewRecorder()
	b.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/provision", nil))
	if r.Code != 404 {
		t.Fatal(r.Code)
	}
}
func TestProvisionUsesVerifiedUIDNotUntrustedBody(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	if current.Uid == "0" {
		t.Skip("CI service user needed to test eligibility via NSS")
	}
	groups, err := userGroups(current)
	if err != nil {
		t.Skip(err)
	}
	p := examplePolicy()
	p.EligibleGroups = groups
	f := newFakeSite()
	b := &broker{policy: p, verify: func(context.Context, string) (identityProof, error) {
		return identityProof{uid: current.Uid, profile: "ai-special"}, nil
	}, backend: commandBackend{run: f.run}}
	r := httptest.NewRecorder()
	b.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/provision", strings.NewReader(`{"credential":"signed-placeholder"}`)))
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if got := f.accounts[current.Username+"-ai-special"]; !reflect.DeepEqual(got, []string{"agents-special"}) {
		t.Fatalf("backend provisioned %v", f.accounts)
	}
}
func TestParseMungeOutput(t *testing.T) {
	proof, err := parseMungeOutput("STATUS: Success (0)\nUID: 1234\nGID: 200\n\nchaperone:v1:ai-special\n")
	if err != nil || proof.uid != "1234" || proof.profile != "ai-special" {
		t.Fatalf("%+v %v", proof, err)
	}
	// Real unmunge output on Rocky 9 (munge 0.5.13, no -N): names with numbers.
	proof, err = parseMungeOutput("STATUS:           Success (0)\nENCODE_HOST:      login1.example.org (192.0.2.10)\nTTL:              300\nUID:              alice (1892600014)\nGID:              alice (1892600014)\nLENGTH:           15\n\nchaperone:v1:ai")
	if err != nil || proof.uid != "1892600014" || proof.profile != "ai" {
		t.Fatalf("real Rocky 9 unmunge output: %+v %v", proof, err)
	}
	// Real `unmunge -N` output (munge 0.5.15): numeric status and UID.
	proof, err = parseMungeOutput("STATUS:          0\nENCODE_HOST:     172.17.0.2\nUID:             1234\nGID:             200\nLENGTH:          15\n\nchaperone:v1:ai")
	if err != nil || proof.uid != "1234" || proof.profile != "ai" {
		t.Fatalf("real unmunge -N output: %+v %v", proof, err)
	}
	for _, input := range []string{
		"STATUS: Failure\nUID: 1234\n\nchaperone:v1:ai",
		"STATUS:          17\nUID: 1234\n\nchaperone:v1:ai", // replayed, numeric
		"STATUS: Success\nUID: bad\n\nchaperone:v1:ai",
		"STATUS: Success\nUID: alice (x)\n\nchaperone:v1:ai",
		"STATUS: Success\nUID: 1234\n\nbad-payload",
		"STATUS: Success\nUID: 1234\n\nchaperone:v1:../../root",
		"STATUS: Success\nUID:\n\nchaperone:v1:ai",
		"UID: 1234\n\nchaperone:v1:ai", // missing status
		"STATUS: Success\nUID: 1234",   // no payload separator
	} {
		if _, err := parseMungeOutput(input); err == nil {
			t.Errorf("accepted invalid credential output %q", input)
		}
	}
}
func TestParseIPA(t *testing.T) {
	got := parseIPA("  cn: alice-ai\n  member: uid=alice,cn=users\n  Member: uid=bob,cn=users\n  description: chaperone:alice\n")
	if !reflect.DeepEqual(got["member"], []string{"uid=alice,cn=users", "uid=bob,cn=users"}) || first(got["description"]) != "chaperone:alice" || first(got["nope"]) != "" {
		t.Fatalf("%v", got)
	}
}

// fakeSite simulates enough ipa/sacctmgr CLI behaviour to exercise the real
// commandBackend.ensure logic without FreeIPA or Slurm.
type fakeSite struct {
	users    map[string][3]string       // persona -> {homedirectory, description, gidnumber}
	groups   map[string][2]string       // group -> {description, gidnumber}
	members  map[string]map[string]bool // group -> member users
	memberOf map[string][]string        // persona -> extra memberOf DNs
	accounts map[string][]string        // persona -> Slurm accounts
	showErr  string                     // non-"not found" show failure
	calls    []string
}

func newFakeSite() *fakeSite {
	return &fakeSite{users: map[string][3]string{}, groups: map[string][2]string{}, members: map[string]map[string]bool{}, memberOf: map[string][]string{}, accounts: map[string][]string{}}
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
	case name == "ipa" && args[0] == "user-show":
		u, ok := f.users[args[1]]
		if !ok {
			return notFound()
		}
		out := "  homedirectory: " + u[0] + "\n  description: " + u[1] + "\n  gidnumber: " + u[2] + "\n  memberof: cn=ipausers,cn=groups,cn=accounts,dc=test\n"
		for _, dn := range f.memberOf[args[1]] {
			out += "  memberOf: " + dn + "\n"
		}
		return out, nil
	case name == "ipa" && args[0] == "user-add":
		if !slices.Contains(args, "--noprivate") {
			return "", errors.New("persona must not get a user private group")
		}
		f.users[args[1]] = [3]string{flag(args, "--homedir="), flag(args, "--setattr=description="), flag(args, "--gidnumber=")}
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

func TestEnsureCreatesPersonaIdempotently(t *testing.T) {
	f := newFakeSite()
	b := commandBackend{run: f.run}
	ctx := context.Background()
	if err := b.ensure(ctx, "alice", "/home/alice", "alice-ai", "agents"); err != nil {
		t.Fatal(err)
	}
	if f.users["alice-ai"] != [3]string{"/home/alice", "chaperone:alice", "5001"} {
		t.Fatalf("identity: %v", f.users["alice-ai"])
	}
	if f.groups["alice-ai"][0] != "chaperone:alice" || !reflect.DeepEqual(f.members["alice-ai"], map[string]bool{"alice": true}) {
		t.Fatalf("persona group: %v %v", f.groups, f.members)
	}
	if !reflect.DeepEqual(f.accounts["alice-ai"], []string{"agents"}) {
		t.Fatalf("accounts: %v", f.accounts["alice-ai"])
	}
	f.calls = nil
	if err := b.ensure(ctx, "alice", "/home/alice", "alice-ai", "agents"); err != nil {
		t.Fatal(err)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("repeat provisioning mutated state: %v", m)
	}
	// The human left its persona's group: added back.
	delete(f.members["alice-ai"], "alice")
	if err := b.ensure(ctx, "alice", "/home/alice", "alice-ai", "agents"); err != nil || !f.members["alice-ai"]["alice"] {
		t.Fatalf("human not re-added: %v %v", err, f.members)
	}
}

func TestEnsureFailsClosed(t *testing.T) {
	ours := func(f *fakeSite) {
		f.groups["alice-ai"] = [2]string{"chaperone:alice", "5001"}
		f.members["alice-ai"] = map[string]bool{"alice": true}
		f.users["alice-ai"] = [3]string{"/home/alice", "chaperone:alice", "5001"}
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
			f.users["alice-ai"] = [3]string{"/home/alice", "chaperone:mallory", "5001"}
		}, "collision"},
		"other primary group": {func(f *fakeSite) {
			ours(f)
			f.users["alice-ai"] = [3]string{"/home/alice", "chaperone:alice", "100"}
		}, "collision"},
		"extra group": {func(f *fakeSite) {
			ours(f)
			f.memberOf["alice-ai"] = []string{"cn=restricted,cn=groups,cn=accounts,dc=test"}
		}, "unexpected IPA membership"},
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
			err := (commandBackend{run: f.run}).ensure(context.Background(), "alice", "/home/alice", "alice-ai", "agents")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Fatalf("mutated state before failing: %v", m)
			}
		})
	}
}

func TestBrokerRejections(t *testing.T) {
	current, err := user.Current()
	if err != nil || current.Uid == "0" {
		t.Skip("needs non-root NSS user")
	}
	groups, err := userGroups(current)
	if err != nil {
		t.Skip(err)
	}
	proof := identityProof{uid: current.Uid, profile: "ai"}
	for name, tc := range map[string]struct {
		eligible []string
		profile  string
		body     string
		showErr  string
		code     int
	}{
		"ineligible human":   {[]string{"no-such-group"}, "ai", `{"credential":"x"}`, "", 403},
		"unknown persona":    {groups, "root", `{"credential":"x"}`, "", 403},
		"unknown JSON field": {groups, "ai", `{"credential":"x","user":"root"}`, "", 400},
		"backend failure":    {groups, "ai", `{"credential":"x"}`, "secret ipa detail", 503},
	} {
		t.Run(name, func(t *testing.T) {
			p := examplePolicy()
			p.EligibleGroups = tc.eligible
			pr := proof
			pr.profile = tc.profile
			f := newFakeSite()
			f.showErr = tc.showErr
			b := &broker{policy: p, verify: func(context.Context, string) (identityProof, error) { return pr, nil }, backend: commandBackend{run: f.run}}
			r := httptest.NewRecorder()
			b.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/provision", strings.NewReader(tc.body)))
			if r.Code != tc.code {
				t.Fatalf("status=%d want %d body=%s", r.Code, tc.code, r.Body.String())
			}
			if strings.Contains(r.Body.String(), "secret") {
				t.Fatal("backend error leaked to client")
			}
			if tc.code != 503 && len(f.calls) != 0 {
				t.Fatal("backend called for rejected request")
			}
		})
	}
}

func TestProvisionClient(t *testing.T) {
	// Go never proxies loopback, so check the transport rather than the env.
	tr := provisionClient.Transport.(*http.Transport)
	if tr.Proxy != nil {
		t.Fatal("provisioning client must not use https_proxy")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "munge"), []byte("#!/bin/sh\nread -r p\necho \"MUNGE:$p\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var got string
	status := 200
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Credential string }
		json.NewDecoder(r.Body).Decode(&req)
		got = r.URL.Path + " " + req.Credential
		w.WriteHeader(status)
	}))
	defer srv.Close()
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	t.Cleanup(func() { tr.TLSClientConfig = nil })
	if err := provision(context.Background(), srv.URL, "ai-special"); err != nil {
		t.Fatal(err)
	}
	if got != "/v1/provision MUNGE:chaperone:v1:ai-special" {
		t.Fatalf("server saw %q", got)
	}
	status = 403
	if err := provision(context.Background(), srv.URL, "ai"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("rejection not reported: %v", err)
	}
}

func TestMungeVerifyAndCmdOutput(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "unmunge"), []byte("#!/bin/sh\ncat >/dev/null\nprintf 'STATUS: Success (0)\\nUID: alice (4242)\\n\\nchaperone:v1:ai\\n'\n"), 0755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	proof, err := mungeVerify(context.Background(), "token")
	if err != nil || proof.uid != "4242" || proof.profile != "ai" {
		t.Fatalf("%+v %v", proof, err)
	}
	if _, err := mungeVerify(context.Background(), ""); err == nil {
		t.Fatal("empty credential accepted")
	}
	out, err := cmdOutput(context.Background(), "sh", "-c", "echo boom; exit 3")
	if err == nil || !strings.Contains(err.Error(), "boom") || out != "boom\n" {
		t.Fatalf("command failure lost its output: %q %v", out, err)
	}
}
