<div align="center">

# Chaperone

**Your AI agents, your environment, their own identity.**

Run Claude Code, Codex, OpenCode, scripts and Slurm jobs on an HPC cluster
under a separate, automatically created Unix identity with a HOME of its
own, your environment, and your harness settings copied in.

[![Go CI](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml)
[![Integration](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml)

</div>

---

```sh
chaperone seed claude codex ~/.config/foo   # copy any tool's state (logins too) to alice-ai
chaperone claude                            # Claude Code as alice-ai
chaperone include -R ~/projects/analysis    # share a directory with alice-ai, on every node
cd ~/projects/analysis && chaperone claude
chaperone sbatch pipeline.sh                # the job runs as alice-ai
chaperone -p proja claude                   # another profile: alice-proja, its own HOME
chaperone                                   # a shell as the persona
```

## Why

An agent running as you holds every group you hold, including those that
open restricted data, and can edit your harness hooks that you later run.
Chaperone runs it as **`<you>-<profile>`**, a real directory user that holds
your groups minus the ones its profile bans, with its own HOME:

| The persona | |
| --- | --- |
| has a HOME of its own, `~/.chaperone/<you>-<profile>`, with its own tool state and logins | its own; you may read and write there |
| has your environment: its login shell reads your startup files | yes |
| starts with your tools' settings and logins | when you `chaperone seed` them (a copy of what the profile lists) |
| reads your `0600` files, `~/.ssh`, your harness state, the rest of HOME | **no**, unless you `include` them |
| holds your groups | all but `ban_groups` |
| submits Slurm jobs under its own account, with the same rights on every node | yes |
| uses sudo or any setuid program, or logs in | **never** |

> [!NOTE]
> Chaperone does not hide data that is world-readable: the persona sees what
> any user on the cluster sees. Fix permissions at the source.

## How it works

```mermaid
sequenceDiagram
    participant H as alice (shell)
    participant E as _enter (root, via sudo)
    participant B as _provision<br/>(service user)
    participant D as FreeIPA + Slurm
    participant P as alice-ai
    H->>E: sudo chaperone _enter
    E->>B: run as the service user
    B->>D: alice eligible? ensure alice-ai (HOME ~/.chaperone/alice-ai), its group, alice-chaperone, alice's groups minus banned, Slurm association
    E->>E: HOME and startup files: group alice-chaperone; create ~/.chaperone/alice-ai if new
    E->>P: run as alice-ai, HOME its own, no_new_privs
    P->>P: claude / codex / sbatch …
    E->>E: session over: kill its leftovers
```

- **One Go binary, no daemon.** It is the client, the root-only `_enter`
  identity switch and the `_provision` broker.
- **A HOME of its own, inside yours.** FreeIPA gives the persona
  `~/.chaperone/<you>-<profile>` (in `~/.chaperone`, root's, `0711`) as HOME, so harnesses keep their state,
  daemons and logins as for any user, and jobs find it on every node. Your
  HOME and startup files carry your **traverse group** `<you>-chaperone`
  (all your personas) with group bits `--x` and `r--`: personas pass
  through HOME and read your startup files, nothing more. No mounts, no
  ACLs.
- **Your environment.** The persona's `.bash_profile` and `.bashrc` load
  yours with your HOME set, then set its own: PATH, modules and conda are
  yours, and it runs your installed tools.
- **State by copy.** `chaperone seed` copies the paths the profile lists
  for a tool (any tool: claude, codex, nvim, ...), as tar as you piped into
  tar as the persona. A list that names a login copies it; tokens rotate,
  so when one copy stops working, seed again or log in as the persona.
- **Data through groups.** The persona holds your groups minus banned
  ones. `chaperone include PATH` gives a path you own the persona's group,
  group rw (setgid on directories); `-R` also what it holds. Plain
  chgrp/chmod, so it works on every node and in jobs. `exclude PATH`
  restores each recorded group and mode.
- **The broker is never root.** `_enter` runs it as the service user with a
  keytab only that user reads. It checks eligibility in FreeIPA on every
  launch, so a revoked user is refused at once, and syncs the persona's
  groups there: jobs get the same groups as the session. A FreeIPA role
  lets it create users and groups and change members of every group that
  no profile bans; in Slurm it coordinates the persona accounts only.
- **Sessions end cleanly.** When the command exits, the persona's leftover
  processes in that session are killed; parallel sessions keep running.
- **Fail closed.** If any check fails, nothing runs, and nothing ever runs as
  you instead.
- **Compute nodes need nothing.** Personas are ordinary SSSD and Slurm users.

## Install

You need FreeIPA (SSSD on all nodes), Slurm with accounting
(`AccountingStorageEnforce=associations`), MUNGE, sudo and tar on the login
nodes. Take the `el9` artifact from CI: the binary and `deploy/`,
sha256-checked. It is built only when every other check of the same commit
passed. The inventory needs three groups:

```ini
[chaperone]            # login nodes
login1.example.org
[chaperone_ipa]        # one FreeIPA server
ipa1.example.org
[chaperone_slurmdbd]   # the slurmdbd host
slurm.example.org
```

```sh
cp deploy/vars.example.yml site.yml        # eligible users, profiles
ansible-playbook -i inventory -e @site.yml deploy/install.yml
```

The playbook stops on any failure on any host, and in this order sets up:

- **Login nodes**: the required tools, then the binary and the policy,
  which must parse before anything else changes.
- **FreeIPA**: the broker's role (no member changes in banned groups) and
  the eligibility group; with `chaperone_users` set, exactly those members
  ([`deploy/ipa.py`](deploy/ipa.py), run as root over LDAPI, no admin
  password).
- **Slurm**: a persona account per profile, with the broker as coordinator
  ([`deploy/slurm.sh`](deploy/slurm.sh)).
- **Login nodes**: the keytab, fetched with the host principal so it never
  travels (and fetched again if it stops working), and last
  [the sudo rule](deploy/chaperone.sudoers).

Re-running is safe. To add or remove a user, edit `chaperone_users` and
re-run. A human installs nothing: the first launch of a profile creates its
persona and HOME. Existing groups or Slurm accounts that Chaperone did not
create are refused, never adopted. [docs/operations.md](docs/operations.md)
covers the pilot order, the blast radius and the kill switch.

## Uninstall

```sh
ansible-playbook -i inventory -e @site.yml deploy/uninstall.yml                          # retire
ansible-playbook -i inventory -e @site.yml -e chaperone_purge=true deploy/uninstall.yml  # purge
```

Both stop everything, in this order, and any failure stops the run:

1. the sudo rule;
2. persona jobs (the run fails if any survive) and Slurm accounts;
3. running sessions;
4. the broker's role, service and the eligibility group in FreeIPA;
5. the binary, configuration, keytab and service user.

**Retire** (default) keeps each persona, disabled, with its groups and its
HOME: what it created stays owned by a named identity you share a group
with. Installing again brings the same personas back. **Purge** also
deletes the personas, their groups and their HOMEs (`~/.chaperone`); their
files elsewhere then keep a bare UID. Both give HOME and each startup file
back the group and mode it had before the first launch. Includes stay:
`chaperone exclude` them first.

## Policy

`/etc/chaperone/policy.json` is root-owned, strict JSON. Each profile is its
own identity `<user>-<profile>`, with its own HOME and logins.

```json
{
  "default_persona": "ai",
  "eligible_groups": ["chaperone-users"],
  "personas": {
    "ai": {
      "slurm_account": "agents",
      "shell": "/bin/bash",
      "ban_groups": ["admins"]
    }
  }
}
```

| Key | Effect |
| --- | --- |
| `ban_groups` | your groups the persona never gets; also kept out of the broker's FreeIPA right |
| `groups` | when set, the persona gets only these of your groups (`[]`: none) |
| `startup_files` | HOME-relative files shared read-only with your traverse group, so the persona's login shell reads them; unset: `.bash_profile`, `.bash_login`, `.profile`, `.bashrc`; `[]`: none |
| `list_home` | HOME's group bits `r-x`, not `--x`: personas list HOME's names; each entry's own mode still decides what they read |
| `shell` | absolute path of a login shell: the command runs through `shell -l`, which reads your startup files; unset, the command runs as given and a command is required |
| `env` | variables set for the persona; `$HOME` is your HOME; `PATH` also finds the command |
| `slurm_account` | the persona's Slurm account: its limits, QOS and partitions |
| `slurm_limits` | sacctmgr limits install sets on that account, e.g. `{"MaxJobs": "50", "GrpTRES": "cpu=64"}`; none by default, `-1` clears one |

No profile may be named `chaperone` (the traverse group's name). Eligibility
groups and your persona groups are never passed on, nor is your primary
group. Group changes, yours or the policy's, apply at the next launch; jobs
see them once SSSD and Slurm's group cache refresh (`entry_cache_timeout`,
`GroupUpdateTime`).

`chaperone [-p PROFILE] include [-R] PATH` refuses HOME itself,
`~/.config/chaperone`, `~/.chaperone`, and a tree holding a banned group,
another filesystem or an entry not yours; symlinks are left alone. Each
change is recorded first in `~/.config/chaperone/includes` (`ino gid mode
PATH`), and applied through a descriptor, only to the inode the walk saw: a
persona that writes an included directory may swap its entries. Without
`-R` only PATH opens; what you create in it later follows your umask (`002`
to share). The persona must also be able to pass through PATH's parents.
Like any chgrp, a ban here is cooperative: you may still chgrp by hand.
Your session needs the persona's group: log in again after your first
launch (Chaperone refreshes your SSSD entry then).

`chaperone [-p PROFILE] seed TOOL|PATH ...` copies into the persona's HOME,
overwriting its copies. A path (with a `/` or a leading `.`) must be in your
HOME. A tool name is any tool: Chaperone looks in `~/.NAME`, `~/.NAME.json`,
`~/.NAMErc`, `~/.config/NAME`, `~/.local/share/NAME` and
`~/.local/state/NAME`, and on a terminal asks per path found. `*.pid`,
`*.sock` and `*.lock` are skipped: they name your processes. Logins are
copied with the rest; tokens rotate, so when one copy stops working, seed
again or log in as the persona. Your tools run from your installs; turn
their self-updaters off in `env` (they cannot write your install).

The persona does not inherit your terminal's environment: sudo resets it,
then the profile's `shell` and `env` build it, as an SSH login would. A
`.bashrc` that returns early when not interactive skips the rest for
`chaperone <command>`. Each launch gives your startup files your traverse
group with group bits `r--` (uninstall gives back the old group and mode);
symlinked startup files are left alone. The persona's own startup files are
written once, when its HOME is made (bash, sh and zsh loaders); they are
its own afterwards. A launch from your HOME starts in the persona's: tools
read `./.TOOL/` as project config, and yours is closed to the persona.

## Trust model

- **Chaperone is a separate identity, not a sandbox.** In what you include
  or share through groups, the persona has those rights.
- **Your harness state stays yours.** The persona has its own; it cannot
  edit your hooks, MCP servers or settings, nor reach your logins or
  daemons.
- **A ban covers the persona, not you.** A banned-group file you move into
  an included directory becomes readable to the persona.
- **The persona owns what it creates.** Its group, of which you are a
  member, gives you read-write; the persona could still `chmod 600`. You can
  always run `chaperone -- …` to act as it.
- **Unchaperoned agents are allowed.** Chaperone protects people who use it.
  It does not police those who don't.

## Repository layout

```text
cmd/chaperone/        the one binary, package main
  main.go             client: options, include/exclude, seed, then exec sudo
  enter.go            _enter: provision, check, groups, HOMEs, environment
  session.go          the persona's session and its cleanup
  broker.go           _provision: FreeIPA and Slurm reconcile as the service user
  policy.go           /etc/chaperone/policy.json, the group filter
  *_test.go           unit tests next to the code they test
deploy/               everything a site runs
  install.yml         Ansible: login nodes, FreeIPA, Slurm
  uninstall.yml       Ansible: retire (default) or purge
  vars.example.yml    site variables
  ipa.py, slurm.sh    FreeIPA and Slurm objects (used by the playbooks and CI)
  chaperone.sudoers   the sudo rule
  policy.example.json policy reference
docs/operations.md    pilot order, blast radius, broker privileges, kill switch
docs/PERSONA_HOME.md  the design of the persona's own HOME
docs/archive/         postulates of earlier builds
tests/e2e/            install.yml and uninstall.yml, real UIDs, sudo (CI)
tests/integration/    disposable FreeIPA and Slurm, broker with its real rights (CI)
AGENTS.md             postulates every contributor and agent follows
```

## Development

```sh
make test lint build   # unit tests (race), vet, binary ./chaperone
make secrets           # gitleaks over the whole history
```

CI also runs e2e (the real playbooks, real UIDs, sudo) and integration
(disposable FreeIPA and Slurm), and builds the `el9` artifact. Contributors
and coding agents follow the [postulates](AGENTS.md); tests encode them.

MIT licensed.
