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
	pr, err := b.policy.profile(proof.profile)
	if err != nil {
		http.Error(w, "profile forbidden", 403)
		return
	}
	name, err := personaName(u.Username, proof.profile)
	if err != nil {
		http.Error(w, "invalid name", 403)
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock() // first-use requests are infrequent; serialize entire provision transaction
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	if err := b.backend.ensure(ctx, u.Username, u.HomeDir, name, pr.SlurmAccount); err != nil {
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

// ensure creates or checks the persona: its own POSIX group (description
// "chaperone:<human>", the human a member, so the human controls the
// persona's files), the user with that group as primary group and the
// human's HOME, and one Slurm association. Anything else fails closed.
func (b commandBackend) ensure(ctx context.Context, human, home, persona, account string) error {
	run := b.run
	if run == nil {
		run = cmdOutput
	}
	mark := "chaperone:" + human
	// show returns the object, creating it first if IPA has none. A name
	// owned by something else is never adopted: the marks below must match.
	show := func(kind string, add ...string) (map[string][]string, error) {
		out, err := run(ctx, "ipa", kind+"-show", persona, "--all", "--raw")
		if err != nil && strings.Contains(strings.ToLower(out), "not found") {
			if _, err = run(ctx, "ipa", append([]string{kind + "-add", persona}, add...)...); err != nil {
				return nil, err
			}
			out, err = run(ctx, "ipa", kind+"-show", persona, "--all", "--raw")
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
		"--noprivate", "--gidnumber="+gid, "--setattr=description="+mark)
	if err != nil {
		return err
	}
	if first(u["homedirectory"]) != home || first(u["description"]) != mark || first(u["gidnumber"]) != gid {
		return errors.New("IPA persona collision or mismatched HOME/group")
	}
	// No groups beyond its own: ipausers is FreeIPA's non-POSIX default.
	for _, dn := range u["memberof"] {
		if !strings.HasPrefix(strings.ToLower(dn), "cn=ipausers,cn=groups,") {
			return fmt.Errorf("unexpected IPA membership %q", dn)
		}
	}
	out, err := run(ctx, "sacctmgr", "-n", "-P", "show", "association", "where", "user="+persona, "format=Account")
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
