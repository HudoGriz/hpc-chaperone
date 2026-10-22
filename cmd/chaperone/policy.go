package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
)

const sitePolicy = "/etc/chaperone/policy.json"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var profileRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

type Policy struct {
	Default        string             `json:"default_persona"`
	EligibleGroups []string           `json:"eligible_groups"`
	Personas       map[string]Persona `json:"personas"`
}

// Persona is one profile. The persona holds the human's groups minus
// BanGroups (or only Groups, when set) and traverses the real HOME; the
// directories it binds it sees through a UID mapping, as the human's.
type Persona struct {
	SlurmAccount string            `json:"slurm_account"`
	Shell        string            `json:"shell"`         // run through this login shell (the human's startup files); unset: exec the command
	Env          map[string]string `json:"env"`           // set in the persona's environment; $HOME is the human's HOME
	BanGroups    []string          `json:"ban_groups"`    // never given to the persona
	Groups       []string          `json:"groups"`        // when set, the persona gets only these of the human's groups
	Bind         []string          `json:"bind"`          // HOME-relative directories, mapped read-write
	BindRO       []string          `json:"bind_ro"`       // the same, read-only
	UserBind     bool              `json:"user_bind"`     // the human may add binds (chaperone bind)
	StartupFiles *[]string         `json:"startup_files"` // shared read-only with the persona; unset: the shell's, []: none
	// Slurm limits on the account (install.yml applies them; none by default).
	SlurmLimits map[string]string `json:"slurm_limits"`
}

// Slurm limits are sacctmgr account Key=Value words; -1 clears one (TRES: cpu=-1).
var limitKeyRE = regexp.MustCompile(`^[A-Za-z]+$`)
var limitRE = regexp.MustCompile(`^[A-Za-z0-9=,:./+-]+$`)
var envRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	// ponytail: one profile, as HOME carries one group (its persona's); a
	// traverse group per human when profiles come back.
	if _, ok := p.Personas[p.Default]; !ok || len(p.Personas) != 1 {
		return errors.New("configure exactly one persona, the default")
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
		for k, v := range pr.SlurmLimits {
			if !limitKeyRE.MatchString(k) || !limitRE.MatchString(v) {
				return fmt.Errorf("persona %s: invalid slurm limit %s=%q", name, k, v)
			}
		}
		if pr.Shell != "" && (!filepath.IsAbs(pr.Shell) || filepath.Clean(pr.Shell) != pr.Shell) {
			return fmt.Errorf("persona %s: shell %q is not an absolute path", name, pr.Shell)
		}
		for k, v := range pr.Env {
			if !envRE.MatchString(k) || strings.ContainsRune(v, 0) {
				return fmt.Errorf("persona %s: invalid env %s", name, k)
			}
		}
		for _, g := range append(slices.Clone(pr.BanGroups), pr.Groups...) {
			if !nameRE.MatchString(g) {
				return fmt.Errorf("persona %s: invalid group %q", name, g)
			}
		}
		paths := append(slices.Clone(pr.Bind), pr.BindRO...)
		if pr.StartupFiles != nil {
			paths = append(paths, *pr.StartupFiles...)
		}
		for _, path := range paths {
			if !safeRel(path) {
				return fmt.Errorf("unsafe path %q", path)
			}
		}
	}
	return nil
}

// safeRel: a clean path below HOME, never HOME itself.
func safeRel(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, "../") && !strings.Contains(path, "\\")
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

// personaGroups is what a persona of human holds besides its own group: the
// human's groups minus the profile's ban list (only its allow list, when
// set), never an eligibility group or a persona group of the human.
// ponytail: any group named <human>-<profile-like name> counts as a persona group.
func (p Policy) personaGroups(human string, pr Persona, humanGroups []string) []string {
	var out []string
	for _, g := range humanGroups {
		rest, persona := strings.CutPrefix(g, human+"-")
		if (persona && profileRE.MatchString(rest)) || slices.Contains(p.EligibleGroups, g) ||
			slices.Contains(pr.BanGroups, g) || (pr.Groups != nil && !slices.Contains(pr.Groups, g)) {
			continue
		}
		out = append(out, g)
	}
	return out
}
