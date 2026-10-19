package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type identityProof struct {
	uid     string
	profile string
}
type verifier func(context.Context, string) (identityProof, error)

type broker struct {
	policy  Policy
	verify  verifier
	backend commandBackend
	mu      sync.Mutex
}

func (b *broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/provision" || r.Method != http.MethodPost {
		http.Error(w, "not found", 404)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var req struct {
		Credential string `json:"credential"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || len(req.Credential) > 4096 {
		http.Error(w, "invalid request", 400)
		return
	}
	proof, err := b.verify(r.Context(), req.Credential)
	if err != nil {
		http.Error(w, "invalid credential", 401)
		return
	}
	u, err := user.LookupId(proof.uid)
	if err != nil || u.Uid == "0" {
		http.Error(w, "unknown user", 403)
		return
	}
	humanGroups, err := userGroups(u)
	if err != nil || !b.policy.eligible(humanGroups) {
		http.Error(w, "ineligible", 403)
		return
	}
	expected, err := b.policy.groups(privateGroups(u), humanGroups, proof.profile)
	if err != nil {
		http.Error(w, "profile forbidden", 403)
		return
	}
	name, err := personaName(u.Username, proof.profile)
	if err != nil {
		http.Error(w, "invalid name", 403)
		return
	}
	pr := b.policy.Personas[proof.profile]
	b.mu.Lock()
	defer b.mu.Unlock() // first-use requests are infrequent; serialize entire provision transaction
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := b.backend.ensure(ctx, u.Username, u.HomeDir, name, expected, pr.SlurmAccount); err != nil {
		log.Printf("provision failed human=%q profile=%q: %v", u.Username, proof.profile, err)
		http.Error(w, "provisioning failed (see management logs)", 503)
		return
	}
	log.Printf("provisioned human=%q profile=%q persona=%q", u.Username, proof.profile, name)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"persona": name})
}

func mungeVerify(ctx context.Context, credential string) (identityProof, error) {
	var result identityProof
	if credential == "" {
		return result, errors.New("empty credential")
	}
	// No -N: munge 0.5.13 (Rocky 9) lacks it.
	cmd := exec.CommandContext(ctx, "unmunge")
	cmd.Stdin = strings.NewReader(credential)
	out, err := cmd.Output()
	if err != nil {
		return result, fmt.Errorf("unmunge: %w", err)
	}
	return parseMungeOutput(string(out))
}

func parseMungeOutput(text string) (identityProof, error) {
	var result identityProof
	statusOK := false
	header, payload, ok := strings.Cut(text, "\n\n")
	if !ok {
		return result, errors.New("invalid credential output")
	}
	for _, line := range strings.Split(header, "\n") {
		key, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "UID":
			// "alice (1000)"; numeric only with -N.
			if i := strings.LastIndex(val, "("); i >= 0 {
				val = strings.TrimSuffix(val[i+1:], ")")
			}
			if fields := strings.Fields(val); len(fields) > 0 {
				result.uid = fields[0]
			}
		case "STATUS":
			// "Success (0)"; numeric only with -N.
			if val != "0" && !strings.HasPrefix(strings.ToLower(val), "success") {
				return result, errors.New("credential rejected")
			}
			statusOK = true
		}
	}
	if !statusOK {
		return result, errors.New("credential status missing")
	}
	if _, err := strconv.ParseUint(result.uid, 10, 32); err != nil {
		return result, errors.New("missing credential UID")
	}
	payload = strings.TrimSpace(payload)
	if !strings.HasPrefix(payload, "chaperone:v1:") {
		return result, errors.New("bad credential payload")
	}
	result.profile = strings.TrimPrefix(payload, "chaperone:v1:")
	if !profileRE.MatchString(result.profile) {
		return result, errors.New("bad profile")
	}
	return result, nil
}

func runServer(args []string) error {
	// Least privilege: a dedicated user with a scoped IPA role and Slurm
	// coordinator of the persona accounts, never root (root is a Slurm and
	// host superuser).
	if os.Geteuid() == 0 {
		return errors.New("run provisiond as the dedicated chaperone service user, not root")
	}
	if len(args) != 6 || args[0] != "--listen" || args[2] != "--cert" || args[4] != "--key" {
		return errors.New("expected: provisiond --listen HOST:PORT --cert FILE --key FILE")
	}
	p, err := loadPolicy(sitePolicy, true)
	if err != nil {
		return err
	}
	b := &broker{policy: p, verify: mungeVerify, backend: commandBackend{}}
	server := &http.Server{Addr: args[1], Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	log.Printf("chaperone provisioner listening at %s", args[1])
	return server.ListenAndServeTLS(args[3], args[5])
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
func (b commandBackend) ensure(ctx context.Context, human, home, persona string, groups []string, account string) error {
	run := b.run
	if run == nil {
		run = cmdOutput
	}
	// Never overwrite an unrelated IPA principal with a colliding name.
	out, err := run(ctx, "ipa", "user-show", persona, "--all", "--raw")
	if err != nil {
		if !strings.Contains(strings.ToLower(out), "not found") {
			return err
		}
		_, err = run(ctx, "ipa", "user-add", persona, "--first=Chaperone", "--last=Persona", "--homedir="+home, "--shell=/sbin/nologin", "--setattr=description=chaperone:"+human)
		if err != nil {
			return err
		}
		out, err = run(ctx, "ipa", "user-show", persona, "--all", "--raw")
		if err != nil {
			return err
		}
	}
	kv := parseIPA(out)
	if kv["homedirectory"] != home || kv["description"] != "chaperone:"+human {
		return errors.New("IPA persona collision or mismatched HOME")
	}
	got, err := parseIPAMemberships(out)
	if err != nil {
		return err
	}
	// Re-query after each operation at the next launch; unexpected extra groups
	// cause a fail-closed error rather than silently widening access.
	for g := range got {
		if g != "ipausers" && !slices.Contains(groups, g) {
			if _, err := run(ctx, "ipa", "group-remove-member", g, "--users="+persona); err != nil {
				return fmt.Errorf("revoke obsolete group %q: %w", g, err)
			}
		}
	}
	for _, g := range groups {
		if !got[g] {
			if _, err := run(ctx, "ipa", "group-add-member", g, "--users="+persona); err != nil {
				return err
			}
		}
	}
	// Secondary read catches immutable/indirect memberships and failed revocation.
	refreshed, err := run(ctx, "ipa", "user-show", persona, "--all", "--raw")
	if err != nil {
		return err
	}
	actual, err := parseIPAMemberships(refreshed)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if !actual[g] {
			return fmt.Errorf("missing IPA group %q after reconcile", g)
		}
	}
	for g := range actual {
		if g != "ipausers" && !slices.Contains(groups, g) {
			return fmt.Errorf("unexpected group %q after reconcile", g)
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

// parseIPAMemberships accepts FreeIPA's raw LDAP memberOf DNs as well
// as normalized memberof_group fields. Reject unfamiliar direct membership
// types instead of silently omitting potentially privileged memberships.
func parseIPAMemberships(raw string) (map[string]bool, error) {
	groups := make(map[string]bool)
	const groupMarker = ",cn=groups,cn=accounts,"
	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "memberof_group":
			if !nameRE.MatchString(value) {
				return nil, fmt.Errorf("invalid IPA group name %q", value)
			}
			groups[value] = true
		case "memberof":
			lower := strings.ToLower(value)
			sep := strings.Index(lower, groupMarker)
			if !strings.HasPrefix(lower, "cn=") || sep <= 3 || sep+len(groupMarker) >= len(value) {
				return nil, fmt.Errorf("unrecognized IPA direct membership DN %q", value)
			}
			name := value[3:sep]
			if !nameRE.MatchString(name) {
				return nil, fmt.Errorf("invalid IPA membership group %q", name)
			}
			groups[name] = true
		}
	}
	return groups, nil
}

func parseIPA(raw string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok {
			m[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return m
}
