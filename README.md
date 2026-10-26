<p align="center">
  <img src="docs/assets/banner.svg" alt="Chaperone: your AI agents, your environment, their own identity on your HPC. alice launches alice-ai through sudo: its own HOME and logins, alice's groups minus banned ones, its own Slurm account." width="100%">
</p>

<p align="center">
  <a href="https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml"><img alt="Go CI" src="https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml/badge.svg"></a>
  <a href="https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml"><img alt="Integration" src="https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml/badge.svg"></a>
  <a href="#trust-model-and-limits"><img alt="Status: experimental" src="https://img.shields.io/badge/status-experimental-orange"></a>
  <a href="LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-green"></a>
</p>

<p align="center">
  <strong>Launch the agent. It runs as its own user. Share only what it needs. Keep your logins, keys and restricted data yours.</strong>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#using-it">Using it</a> ·
  <a href="#work-in-a-project-together">Projects</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#trust-model-and-limits">Trust model</a> ·
  <a href="#install-administrators">Install</a> ·
  <a href="AGENTS.md">Postulates</a>
</p>

---

Chaperone runs **Claude Code, Codex, OpenCode, any tool, script or Slurm job** on an HPC cluster as **`<you>-<profile>`**: a real directory user, created automatically on first launch, with a HOME of its own, your environment, your groups minus the banned ones, and its own Slurm account. One Go binary, no daemon, nothing to install on compute nodes.

It is **not** a sandbox or a container runtime. It is a separate identity, built from what the cluster already has: FreeIPA, SSSD, Slurm, sudo and Unix groups.

> [!NOTE]
> **Why a separate identity?** An agent running as you holds every group you hold, including those that open restricted data, and can edit the hooks and settings of tools you later run. As its own user it reaches only what its groups and your sharing allow, on the login node and in every job.

## Quick start

As `alice`, on a login node where Chaperone is installed:

```sh
chaperone -- true                           # first launch: creates alice-ai and its HOME
chaperone seed claude codex gh              # copy those tools' state into alice-ai's HOME
chaperone claude                            # Claude Code as alice-ai
cd /data/project && chaperone codex         # works where your groups reach
chaperone sbatch pipeline.sh                # the job runs as alice-ai, on any node
chaperone -p proja claude                   # another profile: alice-proja, its own HOME
chaperone                                   # a shell as the persona
```

> [!TIP]
> Log in again after the first launch: your session then holds the persona's group, so you can write what it creates and share your own directories with it.

| The persona | |
| --- | --- |
| has a HOME of its own, `~/.chaperone/<you>-<profile>`, with its own tool state, daemons and logins | yes; you may read and write there |
| has your environment: its login shell reads your startup files | yes |
| starts with your tools' settings and logins | when you `chaperone seed` them (a copy) |
| reads your `0600` files, `~/.ssh`, your tools' state, the rest of HOME | **no**, unless you share them through a group |
| holds your groups | all but the profile's `ban_groups` |
| submits Slurm jobs under its own account, with the same rights on every node | yes |
| uses sudo or any setuid program, or logs in | **never** |

## What lives where

```text
~/                                  you; group alice-chaperone, --x (r-x with list_home)
├── .bashrc  .profile  …            your startup files, r-- for your personas
├── .ssh/  .claude/  .codex/  …     yours alone
├── .chaperone/                     root, 0711: only root changes its entries
│   ├── alice-ai/                   alice-ai's HOME (2770, you are in its group)
│   │   ├── .bashrc  .profile  …    load yours with your HOME, then its own applies
│   │   └── .claude/  .codex/  …    its own tool state and logins, seeded from yours
│   └── alice-proja/                another profile, another persona
└── projects/analysis/              shared: group alice-ai, g+rwX, setgid
```

## Using it

### Launch

`chaperone [-p PROFILE] [--] [COMMAND ...]` runs COMMAND as the profile's persona; with no command, a login shell. The first launch of a profile creates the persona, its group and its HOME; later launches sync its groups with yours. Exit codes and signals pass through; when the command ends, the persona's leftover processes in that session are killed.

The persona's environment is built like an SSH login, not inherited from your terminal: its `.bash_profile`, `.bashrc`, `.profile`, `.zprofile` and `.zshrc` load yours with your HOME set, then its own HOME applies. PATH, modules and conda are yours, and it runs your installed tools (it cannot write them: turn self-updaters off in the profile's `env`). A launch from your HOME starts in the persona's HOME: tools read `./.TOOL/` as project config, and yours is closed to the persona.

### Copy tool state: `seed`

```sh
chaperone seed nvim opencode            # a tool name
chaperone seed ~/.config/foo ./notes    # paths in your HOME
```

Chaperone is tool-neutral: nothing is predefined. A name is looked up where tools keep state:

| Looked up for `NAME` | |
| --- | --- |
| `~/.NAME` `~/.NAME.json` `~/.NAMErc` | dotfiles and dot-directories |
| `~/.config/NAME` `~/.local/share/NAME` `~/.local/state/NAME` | XDG config, data and state |

On a terminal it asks for each path found. An argument with a `/` or a leading `.` is a path and must be in your HOME. It runs as tar as you, piped into tar as the persona: neither side reaches what its user may not, and symlinks are copied as symlinks. A re-seed overwrites the persona's copies.

`*.pid`, `*.sock` and `*.lock` are never copied: they name your processes, and a tool trusting them would wait on your daemon instead of starting its own.

> [!IMPORTANT]
> Logins are copied with the rest, but a copied login shares one refresh-token chain with yours: when one side refreshes, the other copy stops working. For a login that lasts, log in as the persona (`chaperone claude` then `/login`, `chaperone codex login --device-auth`).

**Common seeds:**

| Seed | Brings |
| --- | --- |
| `chaperone seed claude codex opencode` | agent settings, instructions, skills, plugins (and logins) |
| `chaperone seed gh git ~/.gitconfig` | GitHub CLI login, git config and identity |
| `chaperone seed nvim` | editor configuration and plugins |

`~/.gitconfig` is not under a looked-up place: name it by path. If it signs commits (`commit.gpgsign`), the persona has no key. **Recommended:** give it a signing key of its own, so its commits are verified yet told apart from yours and revoked alone:

```sh
chaperone -- sh -c 'mkdir -p ~/.ssh && ssh-keygen -t ed25519 -N "" -f ~/.ssh/git-signing'
chaperone -- git config --global gpg.format ssh
chaperone -- git config --global user.signingkey '~/.ssh/git-signing.pub'
```

then add that public key to your Git host as a signing key. Or turn signing off for the persona (`chaperone -- git config --global commit.gpgsign false`).

> [!CAUTION]
> Possible, not recommended: `chaperone seed gnupg` copies `~/.gnupg`, your private keys included. The persona, and every agent running as it, can then sign as you anywhere and decrypt what is encrypted to you; without a passphrase it never asks. Revoking means revoking your own key.

### Share data: groups

The persona holds your groups minus the banned ones: what a project group opens, it opens to the persona too. A ban covers the persona, not you: do not give it banned data.

## Work in a project together

From a login that holds the persona's group (`id` lists `alice-ai`):

1. **Give the project the persona's group**, group read-write, setgid on directories so new files keep the group. Symlinks get the group only: chmod would follow them.

   ```sh
   D=~/projects/analysis G=alice-ai
   find "$D" -xdev \( -type l -exec chgrp -h $G {} + \) \
     -o \( -type d -exec chgrp $G {} + -exec chmod g+rwxs {} + \) \
     -o \( -exec chgrp $G {} + -exec chmod g+rwX {} + \)
   ```

   Leave out what you run yourself, such as conda or virtualenv environments: a persona that can write them can make you run its code (prune them: `find "$D" -xdev -path "$D/env" -prune -o …`). Chgrp replaces the old group: whoever reached the path through it loses that access. A path under HOME also needs its parents passable (`list_home`, or `o+x`).
2. **Keep your own new files writable for it:** `umask 002` where you work there; with `022` the persona can only read them.
3. **A git repository** both of you commit to:

   ```sh
   git -C "$D" config core.sharedRepository group               # git keeps group write
   chaperone -- git config --global --add safe.directory "$D"   # the persona trusts your repo
   chaperone seed gh git ~/.gitconfig                            # optional: login and identity
   ```

4. **Start:** `cd "$D" && chaperone claude`.

Undo: `chgrp -R <your group> "$D"; chmod -R g-w,g-s "$D"`.

> [!WARNING]
> - **Your git runs its hooks.** The persona can write `.git/config` and `.git/hooks`, which your own git would run as you. In that repository run git as `chaperone -- git …`, or `git -c core.hooksPath=/dev/null -c core.fsmonitor=false …`. The same holds for any script there you run yourself.
> - **Large trees are expensive.** On a network filesystem every entry costs metadata operations. Share what the agent writes, not bulk outputs it only reads (it reads those as others may).

## How it works

```mermaid
sequenceDiagram
    participant H as alice (shell)
    participant E as _enter (root, via sudo)
    participant B as _provision (service user)
    participant D as FreeIPA + Slurm
    participant P as alice-ai
    H->>E: sudo chaperone _enter
    E->>B: run as the service user
    B->>D: alice eligible? Ensure alice-ai, its group, alice-chaperone, alice's groups minus banned, Slurm association
    E->>E: HOME and startup files get group alice-chaperone, create ~/.chaperone/alice-ai if new
    E->>P: run as alice-ai, HOME its own, no_new_privs
    P->>P: claude, codex, sbatch …
    E->>E: session over, kill its leftovers
```

| Piece | What it does |
| --- | --- |
| **One Go binary, no daemon** | the client, the root-only `_enter` identity switch and the `_provision` broker |
| **A HOME of its own, inside yours** | FreeIPA gives the persona `~/.chaperone/<you>-<profile>`, so tools keep state, daemons and logins as for any user, and jobs find it on every node; built under a random name through descriptors that never follow a symlink, then renamed into place |
| **A traverse group** | your HOME and startup files carry `<you>-chaperone` (all your personas) with group bits `--x` (`r-x` with `list_home`) and `r--`; the old group and mode are recorded and given back by uninstall; no mounts, no ACLs |
| **Data through groups** | the persona holds your groups minus banned ones, synced in FreeIPA on every launch, so jobs get the same groups as the session; its own group, of which you are a member, keeps what it creates writable by you (umask `007`) |
| **A broker that is never root** | `_enter` runs it as the service user with a keytab only that user reads; it checks eligibility on every launch, so a revoked user is refused at once; FreeIPA lets it change members of unbanned groups only, Slurm lets it coordinate the persona accounts only |
| **Fail closed** | if identity, groups or HOME do not check out, nothing runs, and nothing ever runs as you instead |

## Why this layer exists

| Existing component | Keep using it for | Chaperone adds |
| --- | --- | --- |
| **FreeIPA / SSSD** | identities and groups | a persona per human and profile, created on first launch, its groups synced on every launch |
| **Slurm** | scheduling and accounting | the persona's own association under a persona account, with its limits |
| **sudo** | switching identity | one rule, `chaperone _enter`, and a root half that checks before it switches |
| **Unix groups** | data access | the same rights in sessions and jobs, on every node; bans the persona never crosses |
| **Claude Code / Codex / any tool** | the agent's work | its own HOME, state and logins, seeded from yours on request |

## Trust model and limits

- **A separate identity, not a sandbox.** In what you share through groups, the persona has those rights.
- **Your tools' state stays yours.** The persona cannot edit your hooks, MCP servers or settings, nor reach your logins, sockets or daemons, unless you seed or share them.
- **A ban covers the persona, not you.** A banned-group file you chgrp or move into a shared directory becomes readable to the persona.
- **The persona owns what it creates.** Its group gives you read-write; the persona could still `chmod 600`. `chaperone -- …` acts as it.
- **World-readable is world-readable.** The persona sees what any user on the cluster sees. Fix permissions at the source.
- **Unchaperoned agents are allowed.** Chaperone protects people who use it; it does not police those who don't.
- **Shells:** bash, sh and zsh startup files are loaded; fish and csh are not.
- **Uninstall leaves your chgrps:** a purge leaves paths you gave a persona's group with a bare GID.

Chaperone is **experimental**. CI passing is not a security certification of a site: pilot it on a test account first ([docs/operations.md](docs/operations.md)).

## Install (administrators)

You need FreeIPA (SSSD on all nodes), Slurm with accounting (`AccountingStorageEnforce=associations`), MUNGE, sudo and tar on the login nodes. Take the `el9` artifact from CI: the binary and `deploy/`, sha256-checked, built only when every other check of the same commit passed. The inventory needs three groups:

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

| Step | Sets up |
| --- | --- |
| **Login nodes** | the required tools, then the binary and the policy, which must parse before anything else changes |
| **FreeIPA** | the broker's role (no member changes in banned groups) and the eligibility group; with `chaperone_users` set, exactly those members ([`deploy/ipa.py`](deploy/ipa.py), run as root over LDAPI, no admin password) |
| **Slurm** | a persona account per profile, with the broker as coordinator ([`deploy/slurm.sh`](deploy/slurm.sh)) |
| **Login nodes** | the keytab, fetched with the host principal so it never travels (and fetched again if it stops working), and last [the sudo rule](deploy/chaperone.sudoers) |

Re-running is safe. To add or remove a user, edit `chaperone_users` and re-run. Users install nothing: the first launch of a profile creates its persona and HOME. Existing groups or Slurm accounts that Chaperone did not create are refused, never adopted. [docs/operations.md](docs/operations.md) covers the pilot order, the blast radius and the kill switch.

### Policy

`/etc/chaperone/policy.json` is root-owned, strict JSON (unknown keys are refused). Each profile is its own identity `<user>-<profile>`.

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

No profile may be named `chaperone` (the traverse group's name). Eligibility groups, persona groups and the human's primary group are never passed on. Group changes, the human's or the policy's, apply at the next launch; jobs see them once SSSD and Slurm's group cache refresh (`entry_cache_timeout`, `GroupUpdateTime`).

### Uninstall

```sh
ansible-playbook -i inventory -e @site.yml deploy/uninstall.yml                          # retire
ansible-playbook -i inventory -e @site.yml -e chaperone_purge=true deploy/uninstall.yml  # purge
```

Both stop everything, in this order, and any failure stops the run: the sudo rule; persona jobs (the run fails if any survive) and Slurm accounts; running sessions; the broker's role, service and the eligibility group in FreeIPA; the binary, configuration, keytab and service user.

| Mode | Personas | Their HOMEs |
| --- | --- | --- |
| **Retire** (default) | kept, disabled, with their groups: what they created stays owned by a named identity its human shares a group with; installing again brings them back | kept |
| **Purge** | deleted with their groups; their files elsewhere keep a bare UID | deleted (`~/.chaperone`, listed from FreeIPA, so also after a retire) |

Both give each HOME and startup file back the group and mode it had before the first launch.

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

CI also runs e2e (the real playbooks, real UIDs, sudo) and integration (disposable FreeIPA and Slurm), and builds the `el9` artifact. Contributors and coding agents follow the [postulates](AGENTS.md); tests encode them.

MIT licensed.
