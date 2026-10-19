package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"strings"
	"sync"
	"testing"
)

type fakeBackend struct {
	mu                       sync.Mutex
	count                    int
	lastPersona, lastAccount string
}

func (f *fakeBackend) ensure(_ context.Context, _, _, persona string, _ []string, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	f.lastPersona = persona
	f.lastAccount = account
	return nil
}
func TestProvisionEndpointRejectsInvalidProof(t *testing.T) {
	b := &broker{policy: examplePolicy(), verify: func(context.Context, string) (identityProof, error) { return identityProof{}, errors.New("bad") }, backend: &fakeBackend{}}
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
	f := &fakeBackend{}
	b := &broker{policy: p, verify: func(context.Context, string) (identityProof, error) {
		return identityProof{uid: current.Uid, profile: "ai-special"}, nil
	}, backend: f}
	r := httptest.NewRecorder()
	b.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/provision", strings.NewReader(`{"credential":"signed-placeholder"}`)))
	if r.Code != 200 {
		t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
	}
	if f.lastPersona != current.Username+"-ai-special" || f.lastAccount != "agents-special" {
		t.Fatalf("backend: %+v", f)
	}
}
func TestIPAOutputParsing(t *testing.T) {
	s := "  homedirectory: /home/alice\n  description: chaperone:alice\n"
	k := parseIPA(s)
	if k["homedirectory"] != "/home/alice" || k["description"] != "chaperone:alice" {
		t.Fatal(k)
	}
}
func TestFailClosedGroupAndHome(t *testing.T) {
	p := examplePolicy()
	if p.eligible([]string{"other"}) {
		t.Fatal("unexpected eligibility")
	}
	if _, err := p.groups([]string{"research"}, "nonexistent"); err == nil {
		t.Fatal("accepted unknown persona")
	}
	_ = os.ErrPermission
}

func TestParseMungeOutput(t *testing.T) {
	proof, err := parseMungeOutput("STATUS: Success (0)\nUID: 1234\nGID: 200\n\nchaperone:v1:ai-special\n")
	if err != nil || proof.uid != "1234" || proof.profile != "ai-special" {
		t.Fatalf("%+v %v", proof, err)
	}
	for _, input := range []string{
		"STATUS: Failure\nUID: 1234\n\nchaperone:v1:ai",
		"STATUS: Success\nUID: bad\n\nchaperone:v1:ai",
		"STATUS: Success\nUID: 1234\n\nbad-payload",
		"STATUS: Success\nUID: 1234\n\nchaperone:v1:../../root",
		"STATUS: Success\nUID:\n\nchaperone:v1:ai",
	} {
		if _, err := parseMungeOutput(input); err == nil {
			t.Errorf("accepted invalid credential output %q", input)
		}
	}
}
func TestMungeMissingStatus(t *testing.T) {
	if _, err := parseMungeOutput("UID: 1234\n\nchaperone:v1:ai"); err == nil {
		t.Fatal("missing status accepted")
	}
}

func TestParseIPAMemberships(t *testing.T) {
	raw := "memberof: cn=ipausers,cn=groups,cn=accounts,dc=chaperone,dc=test\n" +
		"memberOf: cn=agents,cn=groups,cn=accounts,dc=chaperone,dc=test\n" +
		"memberof: cn=genome-project,cn=groups,cn=accounts,dc=chaperone,dc=test\n"
	got, err := parseIPAMemberships(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"ipausers", "agents", "genome-project"} {
		if !got[g] {
			t.Errorf("raw IPA membership %q missing: %v", g, got)
		}
	}
	normalized, err := parseIPAMemberships("memberof_group: research\n")
	if err != nil || !normalized["research"] {
		t.Fatalf("normalized IPA memberships: %v, error: %v", normalized, err)
	}
	for _, bad := range []string{
		"memberof: cn=admins,cn=roles,cn=accounts,dc=test\n",
		"memberof: cn=../../escape,cn=groups,cn=accounts,dc=test\n",
		"memberof_group: bad name\n",
	} {
		if _, err := parseIPAMemberships(bad); err == nil {
			t.Errorf("accepted unsafe IPA membership: %q", bad)
		}
	}
}

