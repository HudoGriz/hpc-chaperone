<div align="center">

# Chaperone

**Your AI agents, your environment, their own identity.**

Run Claude Code, Codex, OpenCode, any tool, script or Slurm job on an HPC
cluster as a separate, automatically created Unix identity: a HOME of its
own, your environment, and only the data you share.

[![Go CI](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml)
[![Integration](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml)

</div>

---

## Quick start

As `alice`, on a login node where Chaperone is installed:

```sh
chaperone -- true                           # first launch: creates alice-ai and its HOME
chaperone seed claude codex                 # copy those tools' state into alice-ai's HOME
chaperone claude                            # Claude Code as alice-ai (or /login there)
cd /data/project && chaperone codex          # works where your groups reach
chaperone sbatch pipeline.sh                # the job runs as alice-ai, on any node
chaperone -p proja claude                   # another profile: alice-proja, its own HOME
chaperone                                   # a shell as the persona
```

Log in again after the first launch: your session then holds the persona's
group, so you can write what it creates and share with it.

## Why

An agent running as you holds every group you hold, including those that
open restricted data, and can edit the hooks and settings of tools you later
run. Chaperone runs it as **`<you>-<profile>`**, a real directory user:

| The persona | |
| --- | --- |
| has a HOME of its own, `~/.chaperone/<you>-<profile>`, with its own tool state, daemons and logins | yes; you may read and write there |
| has your environment: its login shell reads your startup files | yes |
| starts with your tools' settings and logins | when you `chaperone seed` them (a copy) |
| reads your `0600` files, `~/.ssh`, your tools' state, the rest of HOME | **no**, unless you share them through a group |
| holds your groups | all but the profile's `ban_groups` |
| submits Slurm jobs under its own account, with the same rights on every node | yes |
| uses sudo or any setuid program, or logs in | **never** |

> [!NOTE]
> Chaperone does not hide data that is world-readable: the persona sees what
> any user on the cluster sees. Fix permissions at the source.

## Using it

### Launch

`chaperone [-p PROFILE] [--] [COMMAND ...]` runs COMMAND as the profile's
persona; with no command, a login shell. The first launch of a profile
creates the persona, its group and its HOME; later launches sync its groups
with yours. Exit codes and signals pass through; when the command ends, the
persona's leftover processes in that session are killed.

The persona's environment is built like an SSH login, not inherited from
your terminal: its `.bash_profile`, `.bashrc`, `.profile`, `.zprofile` and
`.zshrc` load yours with your HOME set, then its own HOME applies. PATH,
modules and conda are yours, and it runs your installed tools (it cannot
write them: turn self-updaters off in the profile's `env`). A launch from
your HOME starts in the persona's HOME: tools read `./.TOOL/` as project
config, and yours is closed to the persona.

### Copy tool state: `seed`

```sh
chaperone seed nvim opencode            # a tool name
chaperone seed ~/.config/foo ./notes    # paths in your HOME
```

Chaperone is tool-neutral: nothing is predefined. A name is looked up where
tools keep state, `~/.NAME`, `~/.NAME.json`, `~/.NAMErc`, `~/.config/NAME`,
`~/.local/share/NAME` and `~/.local/state/NAME`, and on a terminal it asks
for each path found. An argument with a `/` or a leading `.` is a path and
must be in your HOME. It runs as tar as you, piped into tar as the persona:
neither side reaches what its user may not, and symlinks are copied as
symlinks. A re-seed overwrites the persona's copies.

`*.pid`, `*.sock` and `*.lock` are never copied: they name your processes,
and a tool trusting them would wait on your daemon instead of starting its
own. Logins are copied with the rest, but a copied login shares one
refresh-token chain with yours: when one side refreshes, the other copy
stops working. For a login that lasts, log in as the persona
(`chaperone claude` then `/login`, `chaperone codex login --device-auth`).

Common seeds:

```sh
chaperone seed claude codex opencode    # agent settings, skills, plugins (and logins)
chaperone seed gh git ~/.gitconfig      # GitHub CLI login, git config and identity
chaperone seed nvim                     # editor config
```

`~/.gitconfig` is not under a looked-up place: name it by path. If it
signs commits (`commit.gpgsign`), the persona has no key. Recommended: give
it a signing key of its own, so its commits are verified yet told apart
from yours and revoked alone:

```sh
chaperone -- sh -c 'mkdir -p ~/.ssh && ssh-keygen -t ed25519 -N "" -f ~/.ssh/git-signing'
chaperone -- git config --global gpg.format ssh
chaperone -- git config --global user.signingkey '~/.ssh/git-signing.pub'
```

then add that public key to your Git host as a signing key. Or turn
signing off for the persona (`chaperone -- git config --global
commit.gpgsign false`).

Possible, not recommended: `chaperone seed gnupg` copies `~/.gnupg`, your
private keys included. The persona, and every agent running as it, can
then sign as you anywhere and decrypt what is encrypted to you; without a
passphrase it never asks. Revoking means revoking your own key.

### Share data: groups

The persona holds your groups minus the banned ones: what a project group
opens, it opens to the persona too. A ban covers the persona, not you: do
not give it banned data.

### Work in a project together

From a login that holds the persona's group (`id` lists `alice-ai`; log in
again after the first launch):

1. **Give the project the persona's group**, group read-write, setgid on
   directories so new files keep the group. Symlinks get the group only:
   chmod would follow them.

   ```sh
   D=~/projects/analysis G=alice-ai
   find "$D" -xdev \( -type l -exec chgrp -h $G {} + \) \
     -o \( -type d -exec chgrp $G {} + -exec chmod g+rwxs {} + \) \
     -o \( -exec chgrp $G {} + -exec chmod g+rwX {} + \)
   ```

   Leave out what you run yourself, such as conda or virtualenv
   environments: a persona that can write them can make you run its code
   (prune them: `find "$D" -xdev -path "$D/env" -prune -o …`). On a
   network filesystem every entry costs metadata operations: share what
   the agent writes, not bulk outputs it only reads (those it reads as
   others may). Chgrp replaces the old group: whoever reached the path
   through it loses that access. A path under HOME also needs its parents
   passable (`list_home`, or `o+x`).
2. **Keep your own new files writable for it:** `umask 002` where you work
   there; with `022` the persona can only read them.
3. **A git repository** both of you commit to:

   ```sh
   git -C "$D" config core.sharedRepository group               # git keeps group write
   chaperone -- git config --global --add safe.directory "$D"   # the persona trusts your repo
   chaperone seed ~/.gitconfig                                   # optional: your git identity
   ```

   The persona can then write `.git/config` and `.git/hooks`, which your
   own git would run as you. In that repository run git as
   `chaperone -- git …`, or `git -c core.hooksPath=/dev/null -c
   core.fsmonitor=false …`. The same holds for any script there you run
   yourself.
4. **Start:** `cd "$D" && chaperone claude`.

Undo: `chgrp -R <your group> "$D"; chmod -R g-w,g-s "$D"`.

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
  `~/.chaperone/<you>-<profile>` as HOME, so tools keep their state,
  daemons and logins as for any user, and jobs find it on every node.
  `~/.chaperone` is root's (`0711`); each persona HOME is built under a
  random name through descriptors that never follow a symlink, then renamed
  into place.
- **A traverse group.** Your HOME and startup files carry
  `<you>-chaperone` (all your personas) with group bits `--x` (`r-x` with
  `list_home`) and `r--`: personas pass through HOME and read your startup
  files, nothing more. The old group and mode are recorded; uninstall gives
  them back. No mounts, no ACLs.
- **Data through groups.** The persona holds your groups minus banned
  ones, synced in FreeIPA on every launch, so jobs get the same groups as
  the session. Its own group, of which you are a member, keeps what it
  creates writable by you (umask `007`).
- **The broker is never root.** `_enter` runs it as the service user with a
  keytab only that user reads. It checks eligibility on every launch, so a
  revoked user is refused at once. A FreeIPA role lets it create users and
  groups and change members of every group no profile bans; in Slurm it
  coordinates the persona accounts only.
- **Fail closed.** If identity, groups or HOME do not check out, nothing
  runs, and nothing ever runs as you instead.
- **Compute nodes need nothing.** Personas are ordinary SSSD and Slurm users.

## Trust model and limits

- **A separate identity, not a sandbox.** In what you share through groups,
  the persona has those rights.
- **Your tools' state stays yours.** The persona cannot edit your hooks, MCP
  servers or settings, nor reach your logins, sockets or daemons.
- **A ban covers the persona, not you.** A banned-group file you chgrp or
  move into a shared directory becomes readable to the persona.
- **The persona owns what it creates.** Its group gives you read-write; the
  persona could still `chmod 600`. `chaperone -- …` acts as it.
- **Unchaperoned agents are allowed.** Chaperone protects people who use it;
  it does not police those who don't.
- **Shells:** bash, sh and zsh startup files are loaded; fish and csh are
  not.
- **Uninstall leaves your chgrps:** a purge leaves paths you gave a
  persona's group with a bare GID.

## Install (administrators)

You need FreeIPA (SSSD on all nodes), Slurm with accounting
(`AccountingStorageEnforce=associations`), MUNGE, sudo and tar on the login
nodes. Take the `el9` artifact from CI: the binary and `deploy/`,
sha256-checked, built only when every other check of the same commit
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
ansible-playbook -i inventory -e @site.yml deploy/install.yml --check
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
re-run. Users install nothing: the first launch of a profile creates its
persona and HOME. Existing groups or Slurm accounts that Chaperone did not
create are refused, never adopted. [docs/operations.md](docs/operations.md)
covers the pilot order, the blast radius and the kill switch.

### Policy

`/etc/chaperone/policy.json` is root-owned, strict JSON (unknown keys are
refused). Each profile is its own identity `<user>-<profile>`.

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
| `ban_groups` | groups the persona never gets; also kept out of the broker's FreeIPA right |
| `groups` | when set, the persona gets only these of the human's groups (`[]`: none) |
| `startup_files` | HOME-relative files shared read-only with the traverse group, so the persona's login shell reads them; unset: `.bash_profile`, `.bash_login`, `.profile`, `.bashrc`; `[]`: none |
| `list_home` | HOME's group bits `r-x`, not `--x`: personas list HOME's names; each entry's own mode still decides what they read. HOME is shared by all of a human's profiles: keep it the same across them |
| `shell` | absolute path of a login shell: the command runs through `shell -l`; unset, the command runs as given and a command is required |
| `env` | variables set for the persona; `$HOME` is the human's HOME; `PATH` also finds the command |
| `slurm_account` | the persona's Slurm account: its limits, QOS and partitions |
| `slurm_limits` | sacctmgr limits install sets on that account, e.g. `{"MaxJobs": "50", "GrpTRES": "cpu=64"}`; none by default, `-1` clears one |

No profile may be named `chaperone` (the traverse group's name). Eligibility
groups, persona groups and the human's primary group are never passed on.
Group changes, the human's or the policy's, apply at the next launch; jobs
see them once SSSD and Slurm's group cache refresh (`entry_cache_timeout`,
`GroupUpdateTime`).

### Uninstall

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
HOME: what it created stays owned by a named identity its human shares a
group with. Installing again brings the same personas back. **Purge** also
deletes the personas, their groups and their HOMEs (`~/.chaperone`, listed
from FreeIPA, so also after a retire); their files elsewhere then keep a
bare UID. Both give each HOME and startup file back the group and mode it
had before the first launch.

## Repository layout

```text
cmd/chaperone/        the one binary, package main
  main.go             client: options, seed, then exec sudo
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
docs/operations.md    pilot order, blast radius, broker privileges, kill switch
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
