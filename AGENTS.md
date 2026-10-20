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
   and the human is a member of it. The human fully controls everything the
   persona creates. The persona never gains the human's data access through
   groups.
4. **The persona reaches only what its profile grants.** Each profile in
   the root-owned policy names its read-write and read-only paths in HOME,
   whether HOME is listable and whether the login shell runs. Fixed for every
   profile: the directory it was launched from is shared read-write (owned
   by the human; never HOME or credential directories); HOME itself is never
   writable; credential paths are never shared. Beyond that it sees only
   what every user sees. World-readable data, in HOME or elsewhere, is its
   owner's responsibility, and Chaperone does not hide it.
5. **The same agent, without friction.** The persona uses the human's
   logins, settings and memories:
   - agent state is writable;
   - an agent never re-onboards;
   - the login environment (proxy, locale, agent variables) reaches the persona.
6. **Changes apply on the next launch.** No cache wait, no manual step: a
   new eligible human just launches, and a revoked one is refused at once.
7. **Uninstall removes everything.** That covers everything install and
   launches created: directory objects, personas, Slurm accounts, the
   service user, files, and ACL entries on users' files. It is tested in CI.
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
