<div align="center">

# Chaperone

**Your AI agents, your environment, their own identity.**

Run Claude Code, Codex, OpenCode, scripts and Slurm jobs on an HPC cluster
under a separate, automatically created Unix identity, with your logins,
settings and memories intact.

[![Go CI](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/ci.yml)
[![Integration](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml/badge.svg)](https://github.com/HudoGriz/hpc-chaperone/actions/workflows/integration.yml)

</div>

---

```sh
cd ~/projects/analysis
chaperone claude                         # Claude Code as alice-ai
chaperone codex exec "summarise results" # Codex as alice-ai
chaperone sbatch pipeline.sh             # the job runs as alice-ai
chaperone -p ai-special -- nextflow run  # another profile, another identity
chaperone                                # a shell as the persona
```

## Why

An agent running as you can read everything you can, including restricted
data your groups open up. Chaperone runs it as **`<you>-ai`**, a real
directory user that holds none of your groups:

| The persona | |
| --- | --- |
| **can** use your agent logins, settings, skills and memories | `~/.claude`, `~/.codex`, … shared read-write |
| **can** run your per-user agent installs | `~/.local/share/claude`, … read-only |
| **can** work in the directory you launched it from | read-write both ways |
| **can** submit Slurm jobs | under its own account, on every node |
| **cannot** read the rest of your HOME, your keys, or your groups' data | |
| **cannot** use sudo, launch personas, or log in | |

**You keep full control** of everything the persona creates. You are a member
of its group, and shared directories carry default ACLs for both of you.

> [!NOTE]
> Chaperone does not hide data that is world-readable: the persona sees what
> any user on the cluster sees. Fix permissions at the source.

## How it works

```mermaid
sequenceDiagram
    participant H as alice (shell)
    participant B as chaperone-provisiond<br/>(service user)
    participant D as FreeIPA + Slurm
    participant P as alice-ai
    H->>B: MUNGE credential (UID + profile) over loopback HTTPS
    B->>D: ensure group alice-ai (alice a member), user alice-ai, Slurm association
    H->>H: setfacl: share agent state + launch dir with alice-ai
    H->>P: sudo chaperone _enter (fresh identity check, drop to alice-ai)
    P->>P: claude / codex / sbatch …
```

- **One Go binary, no dependencies.** It is the client, the root-only
  `_enter` identity switch, the `provisiond` broker and `_uninstall`.
- **The broker is never root.** A FreeIPA role lets it create users and
  groups and change members of persona groups only. In Slurm it coordinates
  the persona accounts only.
- **Fail closed.** If any check fails, nothing runs, and nothing ever runs as
  you instead.
- **Compute nodes need nothing.** Personas are ordinary SSSD and Slurm users.

## Install

You need FreeIPA (SSSD on all nodes), Slurm with accounting, MUNGE, sudo,
and a filesystem with POSIX ACLs. Take the `el9` binary from CI: it is built
and tested on Rocky 9, and the artifact also carries `deploy/ipa.py` and
`deploy/slurm.sh`.

1. **FreeIPA**, as root on an IPA server:
   ```sh
   python3 deploy/ipa.py install login1.example.org chaperone-users alice bob
   ```
2. **Slurm**, as a Slurm admin. The service user needs the same UID on the
   slurmdbd host:
   ```sh
   deploy/slurm.sh install chaperone agents
   ```
3. **Login node**. Install:
   - `/usr/local/bin/chaperone`;
   - `/etc/chaperone/policy.json` (see [`config/policy.example.json`](config/policy.example.json));
   - the broker keytab, fetched with the host principal:
     `ipa-getkeytab -p chaperone/<host> -k /etc/chaperone/chaperone.keytab`;
   - a loopback TLS certificate;
   - [the unit](deploy/chaperone-provisiond.service.example);
   - [the sudo rule](deploy/chaperone.sudoers).

Users in `chaperone-users` can launch from then on.
[docs/operations.md](docs/operations.md) covers the pilot order, the blast
radius and the kill switch.

## Uninstall

Run these in order:

1. Remove the sudo rule.
2. Run `chaperone _uninstall` as root on the login node. It stops persona
   processes, gives persona files to their humans, and removes every ACL
   entry that launches added.
3. Run `deploy/slurm.sh uninstall chaperone agents`.
4. Run `python3 deploy/ipa.py uninstall <host> chaperone-users`. This deletes
   personas, persona groups, the role and the service.
5. Remove the binary, `/etc/chaperone`, the unit, the trust anchor, the
   service user and `/var/log/chaperone-launches`.

CI runs the uninstall against the disposable FreeIPA and Slurm and checks
that nothing is left.

## Policy

`/etc/chaperone/policy.json` is root-owned, strict JSON:

```json
{
  "default_persona": "ai",
  "provision_url": "https://localhost:9443",
  "eligible_groups": ["chaperone-users"],
  "shared_paths": [".claude", ".codex", ".config/opencode"],
  "exec_paths": [".local/bin", ".local/share/claude"],
  "personas": {"ai": {"slurm_account": "agents"}}
}
```

A new profile is one more `personas` entry. Slurm limits, QOS and partitions
belong to its account. Credential directories (`.ssh`, `.gnupg`, `.netrc`,
`.aws`, `.config/gh`) can never be shared.

## Trust model

- **Agent state is shared.** A persona can edit hooks or MCP settings that
  you later run unchaperoned. Treat `~/.claude` and its siblings as your
  trust boundary.
- **Some files need a launch to repair.** A file a tool writes `0600` is
  repaired on the next launch or exit. In a launch directory it stays
  masked until `chaperone -- chmod`.
- **Unchaperoned agents are allowed.** Chaperone protects people who use it.
  It does not police those who don't.

## Development

```sh
make test lint build   # unit tests (race), vet, el9-compatible build
make secrets           # gitleaks over the whole history
```

| Path | What |
| --- | --- |
| `main.go` | client: options, provisioning request, sharing, sudo |
| `broker.go` | `provisiond`: MUNGE check, FreeIPA and Slurm reconcile |
| `enter.go` | `_enter`: fresh identity checks, privilege drop, launch log |
| `acl.go` | sharing with the persona: descriptor-pinned ACL walks |
| `uninstall.go` | `_uninstall`: undo launches on a host |
| `policy.go` | site policy |
| `deploy/` | FreeIPA and Slurm objects, unit, sudo rule |
| `tests/e2e` | real UIDs, sudo, ACLs, MUNGE (CI) |
| `tests/integration` | disposable FreeIPA and Slurm, broker with its real rights (CI) |

Contributors and coding agents follow the [postulates](AGENTS.md).

MIT licensed.
