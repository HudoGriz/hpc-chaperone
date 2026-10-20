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
	"slices"
	"strings"
	"syscall"
)

const sitePolicy = "/etc/chaperone/policy.json"

var nameRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
var profileRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

// protectedPaths below HOME are never shared with a persona, nor anything
// above or below them.
var protectedPaths = []string{".ssh", ".gnupg", ".netrc", ".aws", ".config/gh"}

type Policy struct {
	Default        string             `json:"default_persona"`
	EligibleGroups []string           `json:"eligible_groups"`
	Personas       map[string]Persona `json:"personas"`
}

// Persona is one profile: what its identity reaches in the human's HOME.
// Whatever the profile, it holds no group of the human, and HOME itself is
// never writable.
type Persona struct {
	SlurmAccount string   `json:"slurm_account"`
	ListHome     bool     `json:"list_home"` // HOME r-x (list it, read what any user may); otherwise --x
	Login        bool     `json:"login"`     // run the command through bash -l: the human's startup files
	Write        []string `json:"write"`     // HOME-relative dirs or files, read-write
	Read         []string `json:"read"`      // HOME-relative dirs or files, read-only
	// Slurm limits on the account (install.yml applies them; none by default).
	SlurmLimits map[string]string `json:"slurm_limits"`
}

// slurmLimitKeys are the account limits a profile may set; -1 clears one
// (TRES: cpu=-1).
var slurmLimitKeys = []string{"MaxJobs", "MaxSubmitJobs", "MaxWall", "MaxTRES", "MaxTRESPerJob", "GrpJobs", "GrpSubmitJobs", "GrpTRES", "Fairshare", "QOS", "DefaultQOS"}

var limitRE = regexp.MustCompile(`^[A-Za-z0-9=,:./+-]+$`)

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
			if !slices.Contains(slurmLimitKeys, k) || !limitRE.MatchString(v) {
				return fmt.Errorf("persona %s: invalid slurm limit %s=%q", name, k, v)
			}
		}
		// Limits belong to the account: profiles sharing one must agree.
		if other, ok := limits[pr.SlurmAccount]; ok && !maps.Equal(other, pr.SlurmLimits) {
			return fmt.Errorf("profiles sharing account %s set different slurm_limits", pr.SlurmAccount)
		}
		limits[pr.SlurmAccount] = pr.SlurmLimits
		paths := append(append([]string{}, pr.Write...), pr.Read...)
		for i, path := range paths {
			if path == "" || path == "." || filepath.IsAbs(path) || filepath.Clean(path) != path ||
				path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "\\") {
				return fmt.Errorf("unsafe path %q", path)
			}
			for _, secret := range protectedPaths {
				if inside(path, secret) || inside(secret, path) {
					return fmt.Errorf("path %q touches credentials (%s)", path, secret)
				}
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
