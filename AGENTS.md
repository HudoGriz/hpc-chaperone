# Postulates

Every agent and every human contributor working on this repository follows
these postulates. A change that breaks one is wrong, however well it works.
Changing a postulate is a product decision for the maintainer, not a
side effect of a pull request.

1. **HPC-neutral.** Chaperone runs on any Linux HPC site with FreeIPA (SSSD),
   Slurm and MUNGE. Code, tests and docs name no site: no hostnames, group
   names, paths or data layouts of a particular cluster. Site specifics live
   in the site's `/etc/chaperone/policy.json` and its deployment repository.
2. **A separate identity, failing closed.** Every agent runs as
   `<user>-<profile>`, created automatically on first launch. If identity,
   groups or HOME do not check out, nothing runs. Never fall back to the
   human's UID.
3. **The persona holds no group of the human.** Its only group is its own,
   and the human is a member of it, so the human can work with everything
   the persona creates. That control is cooperative: the persona owns its
   files and could lock them; uninstall hands everything in the recorded
   trees back to the human, without walking whole filesystems. The persona
   never gains the human's data access through groups.
4. **The persona reaches only what its profile grants.** Each profile in
   the root-owned policy names its read-write and read-only paths in HOME,
   whether HOME is listable, whether the login shell runs, and whether
   the launch directory is shared (`workdir: off|auto|required`). Credential
   path access is not hardcoded: site administrators explicitly choose every
   HOME-relative read/write grant. HOME itself is never writable; the launch
   directory cannot be HOME or an ancestor. Beyond that it sees only what
   every user sees. World-readable data is its owner's responsibility.
5. **Profile-defined agent continuity.** The site's default persona can
   reuse the human's logins, settings and memories without built-in rules
   for any particular AI harness:
   - writable state, read-only installations and optional first-use copies
     are explicit in the root-owned profile;
   - only selected environment variables reach a persona;
   - other profiles inherit only the default shell, not credentials, access
     grants or environment settings.
6. **Changes apply on the next launch.** No cache wait, no manual step: a
   new eligible human just launches, and a revoked one is refused at once.
7. **Uninstall leaves nothing running and nothing orphaned.** It removes
   the software, the service user, the broker's rights, the Slurm objects
   and every launch path. By default it retires personas: disabled in the
   directory and kept with their groups, so everything they created stays
   owned by a named identity the human shares a group with, and no file
   tree is walked. Reinstallation does not automatically re-enable them.
   Purge also removes the personas, their groups, and recorded
   ACL entry and persona-owned file in the recorded trees. Both are tested
   in CI.
8. **Least privilege.** The broker:
   - never runs as root (`_enter` runs it as the service user);
   - may only create users and groups and change members of persona groups;
   - is a Slurm coordinator of the persona accounts only.
9. **Ponytail.** Build the smallest thing that works:
   - Go standard library, plus the site tools already there (sudo, setfacl,
     ipa, sacctmgr);
   - one binary, fewest files, fewest features;
   - delete before adding, and no option nobody sets.

   Every security path keeps one test that fails if it breaks.
10. **No credentials or personal data in git.** No secrets, session links,
    hostnames, IP addresses or user names of a site in files, commit
    messages or PRs. `make secrets` (gitleaks plus a session-link check)
    runs in CI and in `.githooks/pre-push`
    (`git config core.hooksPath .githooks`).
11. **Production needs the operator's go.** Prove changes in CI's
    disposable FreeIPA and Slurm first. Never change a site's directory,
    scheduler, sudoers or data without explicit approval.

## Working rules

- **Tests encode the postulates.** When a test fails, check it against the
  postulates first:
  - a test that contradicts a postulate is wrong: change the test, not the code;
  - a test that matches a postulate is right: change the code.

- Read [README.md](README.md) and [docs/operations.md](docs/operations.md) first.
- Pass inputs to commands as `exec.Command` argv; never build shell strings.
- Privileged policy is root-owned at `/etc/chaperone`. Nothing a user sets
  can widen what a persona gets.
- Before a pull request, run `make test lint build`. CI also runs e2e (the
  real playbooks, real UIDs, sudo, ACLs) and integration (disposable FreeIPA
  and Slurm with the broker's real rights).
- Be candid: CI passing is not a security certification of a site.
