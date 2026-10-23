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
3. **The persona holds the human's groups as its profile allows.** By
   default it gets every group of the human except the profile's
   `ban_groups`; a profile may name an allow list (`groups`) instead.
   FreeIPA holds the same memberships, synced on every launch, so jobs get
   them too, and the broker's FreeIPA right excludes every banned group.
   Its own group, of which the human is a member, keeps what it creates
   under the human's control. All personas of a human share one traverse
   group, `<user>-chaperone`, and nothing else of each other.
4. **The persona has a HOME of its own, inside the human's.** FreeIPA gives
   it `~/.chaperone/<user>-<profile>`, created on first launch through
   descriptors that never follow a symlink. The human's HOME carries the
   traverse group with group bits `--x` (`r-x` with `list_home`), each
   startup file with `r--`: personas pass through and read the startup
   files, and see what others may. The human's data reaches the persona
   only through groups and `include`, which gives a path the human owns
   the persona's group (group rw) and records what it changed for
   `exclude`. No mounts, no ACLs; the only walk is `include -R`. The code
   holds no other allow or deny list: what is shared is the policy's and
   the human's decision. Jobs get the same rights on every node.
5. **The same environment, its own harness state.** The persona's login
   shell reads the human's startup files with the human's HOME, then its
   own HOME applies, as an SSH login would. Harness settings come only by
   copy (`chaperone seed`, the profile's lists); logins never: the persona
   logs in itself. Nothing comes from the caller's environment.
6. **Changes apply on the next launch.** No cache wait, no manual step: a
   new eligible human just launches, and a revoked one is refused at once.
7. **Uninstall leaves nothing running and nothing orphaned.** It removes
   the software, the service user, the broker's rights, the Slurm objects
   and every launch path, and ends every session. By default it retires
   personas: disabled in the directory and kept with their groups, so what
   they created stays owned by a named identity the human shares a group
   with, and their HOMEs stay. Purge also removes the personas, their
   groups and their HOMEs.
   Both are tested in CI.
8. **Least privilege.** The broker:
   - never runs as root (`_enter` runs it as the service user);
   - may only create users and groups and change members of groups that
     no profile bans;
   - is a Slurm coordinator of the persona accounts only.
9. **Ponytail.** Build the smallest thing that works:
   - Go standard library, plus established tools (sudo, tar, ipa,
     sacctmgr);
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
  real playbooks, real UIDs, sudo) and integration (disposable FreeIPA
  and Slurm with the broker's real rights).
- Be candid: CI passing is not a security certification of a site.
