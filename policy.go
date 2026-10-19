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
	"sort"
	"strings"
	"syscall"
)

const sitePolicy = "/etc/chaperone/policy.json"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var profileRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

type Policy struct {
	Default        string             `json:"default_persona"`
	ProvisionURL   string             `json:"provision_url"`
	EligibleGroups []string           `json:"eligible_groups"`
	DeniedGroups   []string           `json:"denied_groups"`
	SharedPaths    []string           `json:"shared_paths"`
	Personas       map[string]Persona `json:"personas"`
}
type Persona struct {
	Keep         []string `json:"keep_groups"`
	Add          []string `json:"add_groups"`
	SlurmAccount string   `json:"slurm_account"`
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
	for _, group := range append(append([]string{}, p.DeniedGroups...), p.EligibleGroups...) {
		if !validGroupRule(group) {
			return fmt.Errorf("invalid group %q", group)
		}
	}
	for name, pr := range p.Personas {
		if !profileRE.MatchString(name) || !nameRE.MatchString(pr.SlurmAccount) {
			return fmt.Errorf("invalid persona/account %q", name)
		}
		for _, g := range append(append([]string{}, pr.Keep...), pr.Add...) {
			if !nameRE.MatchString(g) {
				return fmt.Errorf("invalid group %q", g)
			}
		}
		for _, g := range pr.Add {
			if g != "agents" {
				return fmt.Errorf("unsupported privileged group addition %q", g)
			}
		}
	}
	for _, path := range p.SharedPaths {
		if path == ".config/gh" || strings.HasPrefix(path, ".ssh/") || strings.HasPrefix(path, ".gnupg/") || strings.HasPrefix(path, ".netrc/") || strings.HasPrefix(path, ".config/gh/") || strings.HasPrefix(path, ".aws/") || path == ".aws" {
			return fmt.Errorf("protected credential path %q", path)
		}
		if path == "" || strings.HasPrefix(path, "/") || strings.Contains(path, "..") || strings.Contains(path, "\\") || path == ".ssh" || path == ".gnupg" || path == ".netrc" {
			return fmt.Errorf("unsafe shared path %q", path)
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

func (p Policy) groups(humanGroups []string, profile string) ([]string, error) {
	pr, err := p.profile(profile)
	if err != nil {
		return nil, err
	}
	source := make(map[string]bool)
	for _, g := range humanGroups {
		source[g] = true
	}
	denied := func(g string) bool {
		for _, rule := range p.DeniedGroups {
			if rule == g || (strings.HasSuffix(rule, "*") && strings.HasPrefix(g, strings.TrimSuffix(rule, "*"))) {
				return true
			}
		}
		return false
	}
	got := make(map[string]bool)
	for _, g := range pr.Keep {
		if source[g] && !denied(g) {
			got[g] = true
		}
	}
	for _, g := range pr.Add {
		if denied(g) {
			return nil, fmt.Errorf("added group %q is globally denied", g)
		}
		got[g] = true
	}
	out := make([]string, 0, len(got))
	for g := range got {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
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
func (p Policy) eligible(groups []string) bool {
	if len(p.EligibleGroups) == 0 {
		return false
	} // explicit eligibility is mandatory
	for _, g := range p.EligibleGroups {
		if slices.Contains(groups, g) {
			return true
		}
	}
	return false
}

func validGroupRule(g string) bool {
	if strings.HasSuffix(g, "*") {
		return len(g) > 1 && nameRE.MatchString(strings.TrimSuffix(g, "*"))
	}
	return nameRE.MatchString(g)
}
