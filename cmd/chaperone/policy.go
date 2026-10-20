package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const sitePolicy = "/etc/chaperone/policy.json"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var profileRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// protectedPaths below HOME are never shared with a persona.
var protectedPaths = []string{".ssh", ".gnupg", ".netrc", ".aws", ".config/gh"}

type Policy struct {
	Default        string             `json:"default_persona"`
	ProvisionURL   string             `json:"provision_url"`
	EligibleGroups []string           `json:"eligible_groups"`
	SharedPaths    []string           `json:"shared_paths"`
	ExecPaths      []string           `json:"exec_paths"` // read-only, e.g. per-user agent installs
	Personas       map[string]Persona `json:"personas"`
}

// Persona is one profile. It holds no group of the human: only its own group,
// which the human is a member of.
type Persona struct {
	SlurmAccount string `json:"slurm_account"`
}

func loadPolicy(path string, privileged bool) (Policy, error) {
	var p Policy
	fi, err := os.Stat(path)
	if err != nil {
		return p, err
	}
	if privileged {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 {
			return p, errors.New("site policy must be owned by root")
		}
		// The root helper always reads the fixed path, never user-selected policy.
		if fi.Mode().Perm()&0022 != 0 {
			return p, errors.New("site policy must not be group/world writable")
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	if d.Decode(new(any)) != io.EOF {
		return p, errors.New("unexpected trailing JSON")
	}
	return p, p.validate()
}

func (p Policy) validate() error {
	if !profileRE.MatchString(p.Default) {
		return errors.New("invalid default persona")
	}
	if _, ok := p.Personas[p.Default]; !ok {
		return errors.New("default persona not configured")
	}
	for _, group := range p.EligibleGroups {
		if !nameRE.MatchString(group) {
			return fmt.Errorf("invalid group %q", group)
		}
	}
	for name, pr := range p.Personas {
		if !profileRE.MatchString(name) || !nameRE.MatchString(pr.SlurmAccount) {
			return fmt.Errorf("invalid persona/account %q", name)
		}
	}
	for _, path := range append(append([]string{}, p.SharedPaths...), p.ExecPaths...) {
		if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "\\") {
			return fmt.Errorf("unsafe shared path %q", path)
		}
		for _, secret := range protectedPaths {
			if path == secret || strings.HasPrefix(path, secret+"/") {
				return fmt.Errorf("protected credential path %q", path)
			}
		}
	}
	if p.ProvisionURL != "" && !strings.HasPrefix(p.ProvisionURL, "https://") {
		return errors.New("provision_url must use HTTPS")
	}
	return nil
}

func personaName(human, profile string) (string, error) {
	if !nameRE.MatchString(human) || !profileRE.MatchString(profile) || len(human)+len(profile)+1 > 63 {
		return "", errors.New("invalid human or persona name")
	}
	return human + "-" + profile, nil
}

func (p Policy) profile(name string) (Persona, error) {
	pr, ok := p.Personas[name]
	if !ok {
		return pr, fmt.Errorf("unknown persona %q", name)
	}
	return pr, nil
}

func userGroups(u *user.User) ([]string, error) {
	ids, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		g, e := user.LookupGroupId(id)
		if e != nil {
			return nil, e
		}
		names = append(names, g.Name)
	}
	return names, nil
}

// eligible requires explicit eligibility; an empty list admits nobody.
func (p Policy) eligible(groups []string) bool {
	return slices.ContainsFunc(p.EligibleGroups, func(g string) bool { return slices.Contains(groups, g) })
}
