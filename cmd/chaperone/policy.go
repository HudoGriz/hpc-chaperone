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

type Policy struct {
	Default        string             `json:"default_persona"`
	EligibleGroups []string           `json:"eligible_groups"`
	Personas       map[string]Persona `json:"personas"`
}

// Persona is one profile: what its identity reaches in the human's HOME.
// Whatever the profile, it holds no group of the human, and HOME itself is
// never writable.
type SeedFile struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Persona struct {
	SlurmAccount string            `json:"slurm_account"`
	ListHome     bool              `json:"list_home"`   // HOME r-x (list it, read what any user may); otherwise --x
	Workdir      string            `json:"workdir"`     // off (default), auto (share eligible CWD), required (fail if CWD cannot be shared)
	Login        bool              `json:"login"`       // run the command through the configured login shell
	Shell        string            `json:"shell"`       // absolute POSIX-compatible shell; inherited from default persona if unset
	ForwardEnv   []string          `json:"forward_env"` // exact caller variables to preserve
	Env          map[string]string `json:"env"`         // fixed variables; ${HOME} expands to human HOME
	SeedFiles    []SeedFile        `json:"seed_files"`  // optional one-time HOME-relative copies
	Write        []string          `json:"write"`       // HOME-relative dirs or files, read-write
	Read         []string          `json:"read"`        // HOME-relative dirs or files, read-only
	// Slurm limits on the account (install.yml applies them; none by default).
	SlurmLimits map[string]string `json:"slurm_limits"`
}

// slurmLimitKeys are the account limits a profile may set; -1 clears one
// (TRES: cpu=-1).
var slurmLimitKeys = []string{"MaxJobs", "MaxSubmitJobs", "MaxWall", "MaxTRES", "MaxTRESPerJob", "GrpJobs", "GrpSubmitJobs", "GrpTRES", "Fairshare", "QOS", "DefaultQOS"}

var limitRE = regexp.MustCompile(`^[A-Za-z0-9=,:./+-]+$`)
var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var shellRE = regexp.MustCompile(`^/[A-Za-z0-9_./+-]+$`)

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
		if pr.Workdir != "" && pr.Workdir != "off" && pr.Workdir != "auto" && pr.Workdir != "required" {
			return fmt.Errorf("persona %s: workdir must be off, auto or required", name)
		}
		if (name == p.Default && pr.Shell == "") || (pr.Shell != "" && (!shellRE.MatchString(pr.Shell) || filepath.Clean(pr.Shell) != pr.Shell)) {
			return fmt.Errorf("persona %s: shell must be an absolute canonical path (required on the default persona)", name)
		}
		seenEnv := map[string]bool{}
		for _, key := range pr.ForwardEnv {
			if !validEnvName(key) || seenEnv[key] {
				return fmt.Errorf("persona %s: invalid or duplicate forward_env variable %q", name, key)
			}
			seenEnv[key] = true
		}
		for key, value := range pr.Env {
			if !validEnvName(key) || seenEnv[key] || strings.ContainsRune(value, 0) {
				return fmt.Errorf("persona %s: invalid env assignment %q", name, key)
			}
		}
		for _, seed := range pr.SeedFiles {
			if !safeRelativePath(seed.From) || !safeRelativePath(seed.To) || seed.From == seed.To ||
				!slices.ContainsFunc(pr.Write, func(w string) bool { return inside(seed.To, w) && seed.To != w }) {
				return fmt.Errorf("persona %s: invalid seed_files entry %q -> %q (target must be within write paths)", name, seed.From, seed.To)
			}
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
			if !safeRelativePath(path) {
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

// Only the shell is inherited from the configured default. Access grants,
// environment variables and all other profile decisions are NEVER inherited.
func (p Policy) profile(name string) (Persona, error) {
	pr, ok := p.Personas[name]
	if !ok {
		return pr, fmt.Errorf("unknown persona %q", name)
	}
	if pr.Shell == "" && name != p.Default {
		pr.Shell = p.Personas[p.Default].Shell
	}
	return pr, nil
}

// inside reports whether path a is b or below it.
func inside(a, b string) bool {
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, "/")+"/")
}

func safeRelativePath(path string) bool {
	return path != "" && path != "." && path != ".." && !filepath.IsAbs(path) &&
		filepath.Clean(path) == path && !strings.HasPrefix(path, "../") && !strings.Contains(path, "\\")
}

func validEnvName(key string) bool {
	if !envNameRE.MatchString(key) {
		return false
	}
	switch key {
	case "HOME", "USER", "LOGNAME", "PATH":
		return false // Identity and command search path are enforced by Chaperone.
	}
	return true
}
