<div align="center">

# Chaperone

**Your AI agents, your environment, their own identity.**

Run Claude Code, Codex, OpenCode, scripts and Slurm jobs on an HPC cluster
under a separate, automatically created Unix identity, with your logins,
settings and memories exactly as you have them.

[![Go CI](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml)
[![Integration](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml)

</div>

---

```sh
cd ~/projects/analysis
chaperone claude                            # Claude Code as alice-ai
chaperone codex exec "summarise results"    # Codex as alice-ai
chaperone sbatch pipeline.sh                # the job runs as alice-ai
chaperone bind ~/projects/analysis         # this directory, seen as yours
chaperone                                   # a shell as the persona
```

## Why

An agent running as you holds every group you hold, including those that
open restricted data. Chaperone runs it as **`<you>-ai`**, a real directory
user that holds your groups minus the ones its profile bans:

| The persona | |
| --- | --- |
| uses your logins, settings and memories: the directories it binds (`~/.claude`, `~/.codex`, …), seen as yours | read-write; what it writes there is yours |
| reads your startup files and anything in HOME open to others | traverses HOME (`0710`), cannot list it |
| reads your `0600` files, `~/.ssh` and the rest of HOME | **no**, unless bound |
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
    B->>D: alice eligible? ensure alice-ai, its group, alice's groups minus banned, Slurm association
    E->>E: HOME group alice-ai, mode 0710; mount namespace: bindfs maps each bound directory, alice as alice-ai
    E->>P: run as alice-ai, no_new_privs
    P->>P: claude / codex / sbatch …
    E->>E: session over: kill its leftovers, stop bindfs
```

- **One Go binary, no daemon.** It is the client, the root-only `_enter`
  identity switch and the `_provision` broker.
- **Binds, not HOME.** HOME stays the real one; its group becomes the
  persona's with group bits `--x`, so the persona can reach what you bind
  and what is open to others, and nothing more. Each launch gets a mount
  namespace of its own, where [bindfs](https://bindfs.org) shows each
  bound directory with your files as the persona's; what it creates there
  lands owned by you. Locks reach the real files. Nobody else sees the
  mounts. No ACLs; the only walk is one check of a new bind's tree.
- **Slurm needs nothing.** Jobs run as the persona with its groups, on
  every node, also from HOME; binds exist only in the login session.
- **The broker is never root.** `_enter` runs it as the service user with a
  keytab only that user reads. It checks eligibility in FreeIPA on every
  launch, so a revoked user is refused at once, and syncs the persona's
  groups there: jobs get the same groups as the session. A FreeIPA role
  lets it create users and groups and change members of every group that
  no profile bans; in Slurm it coordinates the persona accounts only.
- **Sessions end cleanly.** When the command exits, the persona's leftover
  processes in that session are killed and bindfs stops; parallel sessions
  keep running.
- **Fail closed.** If any check fails, nothing runs, and nothing ever runs as
  you instead.
- **Compute nodes need nothing.** Personas are ordinary SSSD and Slurm users.

## Install

You need FreeIPA (SSSD on all nodes), Slurm with accounting
(`AccountingStorageEnforce=associations`), MUNGE, sudo, and EL9 login nodes
with bindfs (EPEL; install.yml installs it). Take the `el9` artifact from
CI: the binary and `deploy/`, sha256-checked. It is built only when every
other check of the same commit passed. The inventory needs three groups:

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

- **Login nodes**: bindfs and the required tools, then the binary and the
  policy, which must parse before anything else changes.
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
re-run. Existing groups or Slurm accounts that Chaperone did not create are
refused, never adopted. [docs/operations.md](docs/operations.md) covers the
pilot order, the blast radius and the kill switch.

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

**Retire** (default) keeps each persona, disabled, and its group: what it
created outside HOME (scratch, job output) stays owned by a named identity
you share a group with. Installing again brings the same personas back.
**Purge** also deletes the personas and their groups; their files outside
HOME then keep a bare UID. In HOME there is nothing to hand back: the
persona's files there are already yours. Both give each HOME back the
group and mode it had before the first launch. bindfs stays installed.

## Policy

`/etc/chaperone/policy.json` is root-owned, strict JSON. Each profile is its
own identity `<user>-<profile>`. One profile for now: HOME carries one
persona group.

```json
{
  "default_persona": "ai",
  "eligible_groups": ["chaperone-users"],
  "personas": {
    "ai": {
      "slurm_account": "agents",
      "shell": "/bin/bash",
      "ban_groups": ["admins"],
      "bind": [".claude", ".codex", ".config/opencode"],
      "user_bind": true
    }
  }
}
```

| Key | Effect |
| --- | --- |
| `ban_groups` | your groups the persona never gets; also kept out of the broker's FreeIPA right |
| `groups` | when set, the persona gets only these of your groups (`[]`: none) |
| `bind` | HOME-relative directories the persona sees as yours, read-write |
| `bind_ro` | the same, read-only |
| `user_bind` | you may add binds: `chaperone bind [--ro] DIR`, `chaperone unbind DIR`, `chaperone bind` lists |
| `shell` | absolute path of a login shell: the command runs through `shell -l`, which reads your startup files; unset, the command runs as given and a command is required |
| `env` | variables set for the persona; `$HOME` is your HOME; `PATH` also finds the command |
| `slurm_account` | the persona's Slurm account: its limits, QOS and partitions |
| `slurm_limits` | sacctmgr limits install sets on that account, e.g. `{"MaxJobs": "50", "GrpTRES": "cpu=64"}`; none by default, `-1` clears one |

Eligibility groups and your persona groups are never passed on, nor is your
primary group. Group changes, yours or the policy's, apply at the next
launch; jobs see them once SSSD and Slurm's group cache refresh
(`entry_cache_timeout`, `GroupUpdateTime`). A bind is refused if its tree
holds a file or directory of a banned group (in a bind the persona is the
owner, and owner bits ignore the group): `chaperone bind` walks the tree
once. Profile defaults are the policy author's to check. No bind may hold
`~/.config/chaperone`, where your binds are kept. Bind directories, not files: a rename onto a mounted file
fails. There is no built-in deny list: what is bound is the policy's and
your decision.

The persona does not inherit your terminal's environment: sudo resets it,
then the profile's `shell` and `env` build it, as an SSH login would. A
`.bashrc` that returns early when not interactive skips the rest for
`chaperone <command>`. Startup files must be readable by others (`0644`,
the default) for the persona to read them. Files directly in HOME, like
`~/.claude.json`, cannot be bound; with `env: {"CLAUDE_CONFIG_DIR":
"$HOME/.claude"}` Claude Code keeps it in the bound directory (copy yours
there once to keep your onboarding).

## Trust model

- **Chaperone is a separate identity, not a sandbox.** In a bound
  directory the persona has your rights.
- **Bound agent state is shared.** A persona can edit hooks or MCP settings
  (`~/.claude/settings.json`, `~/.codex/config.toml`) that you later run as
  yourself, with all your groups. `bind_ro` closes that, at the price of
  agent state the persona cannot write.
- **A ban covers the persona, not you.** A banned-group file you move into
  a bound directory after its check becomes readable to the persona.
- **Outside HOME the persona owns what it creates.** Its group, of which you
  are a member, gives you read-write; the persona could still `chmod 600`.
- **Unchaperoned agents are allowed.** Chaperone protects people who use it.
  It does not police those who don't.

## Repository layout

```text
cmd/chaperone/        the one binary, package main
  main.go             client: options, then exec sudo
  enter.go            _enter: provision, check, groups, environment
  session.go          the persona's session: mount namespace, binds, cleanup
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
tests/e2e/            install.yml and uninstall.yml, real UIDs, sudo, bindfs (CI)
tests/integration/    disposable FreeIPA and Slurm, broker with its real rights (CI)
AGENTS.md             postulates every contributor and agent follows
```

## Development

```sh
make test lint build   # unit tests (race), vet, binary ./chaperone
make secrets           # gitleaks over the whole history
```

CI also runs e2e (the real playbooks, real UIDs, sudo, bindfs) and
integration (disposable FreeIPA and Slurm), and builds the `el9` artifact.
Contributors and coding agents follow the [postulates](AGENTS.md); tests
encode them.

MIT licensed.
