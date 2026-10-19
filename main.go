// Chaperone is a small identity-aware launcher and central provisioner for Slurm HPC.
// This experimental version is not a substitute for site access-policy validation.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: chaperone [-p PROFILE] [-n] [--] COMMAND [ARGS...]
       chaperone --list-personas | --status
       chaperone provisiond --listen ADDR --cert CERT --key KEY  (admin only)
Default profile is ai. With no command, starts a persona shell.`)
}

type options struct {
	profile           string
	dry, list, status bool
	command           []string
}

func parseArgs(args []string, fallback string) (options, error) {
	o := options{profile: fallback}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			i++
			o.command = args[i:]
			return o, nil
		case a == "-p" || a == "--persona":
			i++
			if i >= len(args) {
				return o, errors.New("missing profile")
			}
			o.profile = args[i]
		case strings.HasPrefix(a, "--persona="):
			o.profile = strings.TrimPrefix(a, "--persona=")
		case a == "-n" || a == "--dry-run":
			o.dry = true
		case a == "--list-personas":
			o.list = true
		case a == "--status":
			o.status = true
		case a == "-h" || a == "--help":
			return o, errors.New("help")
		case strings.HasPrefix(a, "-"):
			return o, fmt.Errorf("unknown option %s (use -- before commands beginning with '-')", a)
		default:
			o.command = args[i:]
			return o, nil
		}
		i++
	}
	return o, nil
}

func main() {
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "provisiond":
		err = runServer(os.Args[2:])
	case len(os.Args) > 1 && os.Args[1] == "_enter":
		err = runEnter(os.Args[2:])
	default:
		if err = runClient(os.Args[1:]); err != nil {
			err = fmt.Errorf("chaperone: %w", err)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runClient(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		usage()
		return nil
	}
	path := sitePolicy
	// Only non-privileged dry-run/inspection can override the site file in tests.
	if os.Geteuid() != 0 && os.Getenv("CHAPERONE_POLICY") != "" {
		path = os.Getenv("CHAPERONE_POLICY")
	}
	p, err := loadPolicy(path, false)
	if err != nil {
		return err
	}
	o, err := parseArgs(args, p.Default)
	if err != nil {
		if err.Error() == "help" {
			usage()
			return nil
		}
		return err
	}
	// A user-chosen policy could widen shared_paths; never use it for a real launch.
	if path != sitePolicy && !o.dry && !o.status && !o.list {
		return errors.New("CHAPERONE_POLICY is only honoured with -n, --status or --list-personas")
	}
	if _, err = p.profile(o.profile); err != nil {
		return err
	}
	if o.list {
		for _, k := range slices.Sorted(maps.Keys(p.Personas)) {
			fmt.Println(k)
		}
		return nil
	}
	current, err := user.Current()
	if err != nil {
		return err
	}
	if current.Uid == "0" {
		return errors.New("launch Chaperone as the human user, not root")
	}
	groups, err := userGroups(current)
	if err != nil {
		return err
	}
	if !p.eligible(groups) {
		return errors.New("user not eligible for Chaperone")
	}
	allowed, err := p.groups(privateGroups(current), groups, o.profile)
	if err != nil {
		return err
	}
	name, err := personaName(current.Username, o.profile)
	if err != nil {
		return err
	}
	if o.status || o.dry {
		fmt.Printf("persona=%s home=%s groups=%s\n", name, current.HomeDir, strings.Join(allowed, ","))
		if o.dry {
			fmt.Printf("argv=%q\n", o.command)
		}
		return nil
	}
	if p.ProvisionURL == "" {
		return errors.New("provision_url missing from site policy")
	}
	// Best effort: without it, group changes wait for the SSSD cache timeout.
	exec.Command("sudo", "-n", "/usr/local/bin/chaperone", "_enter", "--refresh").Run()
	if err := provision(context.Background(), p.ProvisionURL, o.profile); err != nil {
		return err
	}
	if err := shareHome(current, name, p.SharedPaths, true); err != nil {
		return fmt.Errorf("shared home ACL setup: %w", err)
	}
	if err := shareHome(current, name, p.ExecPaths, false); err != nil {
		return fmt.Errorf("exec path ACL setup: %w", err)
	}
	if slices.Contains(p.SharedPaths, ".claude") {
		seedClaudeConfig(current.HomeDir)
	}
	if wd, err := os.Getwd(); err == nil {
		if err := shareWorkdir(current, name, wd); err != nil {
			fmt.Fprintln(os.Stderr, "chaperone: note:", err)
		}
	}
	cmdArgs := []string{"-n", "/usr/local/bin/chaperone", "_enter", "--persona", o.profile, "--path", os.Getenv("PATH"), "--"}
	cmdArgs = append(cmdArgs, o.command...)
	cmd := exec.Command("sudo", cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exitStatus(exit.ProcessState))
		}
		return err
	}
	return nil
}

// seedClaudeConfig copies the human's ~/.claude.json to ~/.claude/.claude.json
// once. The persona cannot write HOME's root, so it runs Claude Code with
// CLAUDE_CONFIG_DIR=~/.claude (set by _enter); the copy carries the human's
// onboarding and settings over. 0660 under the shared default ACL keeps it
// readable and writable for both.
func seedClaudeConfig(home string) {
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(home, ".claude", ".claude.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0660)
	if err != nil {
		return // already seeded
	}
	defer f.Close()
	f.Write(data)
}

// provisionClient ignores https_proxy (exported on HPC hosts) so the MUNGE
// credential goes directly to the broker. Its timeout exceeds the broker's 45s.
var provisionClient = &http.Client{
	Timeout:   60 * time.Second,
	Transport: &http.Transport{TLSHandshakeTimeout: 10 * time.Second},
}

func provision(ctx context.Context, url, profile string) error {
	payload := "chaperone:v1:" + profile
	c := exec.CommandContext(ctx, "munge")
	c.Stdin = strings.NewReader(payload)
	token, err := c.Output()
	if err != nil {
		return fmt.Errorf("munge: %w", err)
	}
	body, _ := json.Marshal(map[string]string{"credential": strings.TrimSpace(string(token))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url+"/v1/provision", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := provisionClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode != 200 {
		return fmt.Errorf("provisioner rejected request (%d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
