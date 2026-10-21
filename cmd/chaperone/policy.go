package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const sitePolicy = "/etc/chaperone/policy.json"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var profileRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

type Policy struct {
	Default        string             `json:"default_persona"`
	EligibleGroups []string           `json:"eligible_groups"`
	WalkLimit      int                `json:"walk_limit"` // entries one first share of a tree may touch
	Personas       map[string]Persona `json:"personas"`
}

// Persona is one profile: everything its identity reaches is set here.
// Whatever the profile, it holds no group of the human, and HOME itself is
// never writable.
type Persona struct {
	SlurmAccount string            `json:"slurm_account"`
	ListHome     bool              `json:"list_home"` // HOME r-x (list it, read what any user may); otherwise --x
	Workdir      bool              `json:"workdir"`   // share the launch directory read-write
	Shell        string            `json:"shell"`     // run through this login shell (the human's startup files); unset: exec the command
	Env          map[string]string `json:"env"`       // set in the persona's environment; $HOME is the human's HOME
	Write        []string          `json:"write"`     // HOME-relative dirs or files, read-write
	Read         []string          `json:"read"`      // HOME-relative dirs or files, read-only
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
	if _, ok := p.Personas[p.Default]; !ok {
		return errors.New("default persona not configured")
	}
	if p.WalkLimit <= 0 {
		return errors.New("walk_limit must be set")
	}
	for _, group := range p.EligibleGroups {
		if !nameRE.MatchString(group) {
			return fmt.Errorf("invalid group %q", group)
		}
	}
	limits := map[string]map[string]string{} // account -> limits
	for name, pr := range p.Personas {
		if !profileRE.MatchString(name) || !nameRE.MatchString(pr.SlurmAccount) {
			return fmt.Errorf("invalid persona/account %q", name)
		}
		for k, v := range pr.SlurmLimits {
			if !limitKeyRE.MatchString(k) || !limitRE.MatchString(v) {
				return fmt.Errorf("persona %s: invalid slurm limit %s=%q", name, k, v)
			}
		}
		// Limits belong to the account: profiles sharing one must agree.
		if other, ok := limits[pr.SlurmAccount]; ok && !maps.Equal(other, pr.SlurmLimits) {
			return fmt.Errorf("profiles sharing account %s set different slurm_limits", pr.SlurmAccount)
		}
		limits[pr.SlurmAccount] = pr.SlurmLimits
		if pr.Shell != "" && (!filepath.IsAbs(pr.Shell) || filepath.Clean(pr.Shell) != pr.Shell) {
			return fmt.Errorf("persona %s: shell %q is not an absolute path", name, pr.Shell)
		}
		for k, v := range pr.Env {
			if !envRE.MatchString(k) || strings.ContainsRune(v, 0) {
				return fmt.Errorf("persona %s: invalid env %s", name, k)
			}
		}
		paths := append(append([]string{}, pr.Write...), pr.Read...)
		for i, path := range paths {
			if path == "" || path == "." || filepath.IsAbs(path) || filepath.Clean(path) != path ||
				path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "\\") {
				return fmt.Errorf("unsafe path %q", path)
			}
			// One grant per object: a nested path would silently override its parent's.
			for _, other := range paths[i+1:] {
				if inside(path, other) || inside(other, path) {
					return fmt.Errorf("persona %s: paths %q and %q overlap", name, path, other)
				}
			}
		}
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

// inside reports whether path a is b or below it.
func inside(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}
