# Chaperone

**Your AI agents. Your environment. Controlled privileges.**

Experimental, small Go launcher for running Claude Code, Codex, OpenCode, scripts and Slurm jobs under automatically provisioned, profile-specific FreeIPA identities. **Not production-ready:** see [deployment gates](#deployment-gates).

## User interface

```sh
chaperone claude                              # default ai profile
chaperone -p ai-special codex exec "test"    # separate FreeIPA UID
chaperone -p ai-special sbatch pipeline.sh   # Slurm sees persona UID
chaperone --list-personas
chaperone --status
chaperone -n -p ai-special -- codex exec "dry run"
```

Personas share the human's actual `HOME`, writable harness configs, skills, OAuth state and memories. The **human** remains responsible for running unchaperoned harnesses or executing agent-modified files. Persona permissions, groups and Slurm account differ.

## Build and test

Requires Go 1.23+ and Linux/NSS (`CGO_ENABLED=1` for FreeIPA/SSSD lookup).

```sh
make test
make build
```

No Go libraries other than the standard library. GitHub Actions runs tests, race detector, vet, formatting and build. Configuration is currently **strict JSON**, rather than TOML: this avoids runtime dependencies in the first implementation. Admins manage `/etc/chaperone/policy.json`; see [`config/policy.example.json`](config/policy.example.json). Profile additions need no code changes.

## Architecture

- One binary: unprivileged `chaperone` client, restricted root-only `_enter` subcommand, `provisiond` HTTPS broker running as a dedicated non-root service user.
- The client obtains a **MUNGE credential** binding its UID and requested profile and sends it over verified HTTPS to the broker.
- The broker decodes it with `unmunge` and derives the human via NSS; no caller-supplied username or account is trusted. It reconciles the FreeIPA user and groups with `ipa` and the Slurm association with `sacctmgr`.
- The client grants the persona ACLs on approved shared-home state and on the directory it is launched from (if the human owns it; never HOME itself or credential dirs), then uses a tightly scoped sudo rule to launch `_enter`. `_enter` refreshes the SSSD entry, independently validates eligibility, exact groups and shared `HOME`, drops privileges, runs the command and waits.
- A launch directory is walked once on first use; afterwards new files inherit both users' default ACL entries, so persona output stays editable by the human without per-launch tree walks. A file a tool creates `0600` there stays masked until `chaperone -- chmod`.
- Shared state survives `0600` rewrites (OAuth refresh): before launch the human re-grants ACLs on its own masked files, after the session the persona does the same for its files. Each identity only touches files it owns; root never walks persona-writable directories.
- Compute-node jobs require no Chaperone daemon; they use normal Slurm and SSSD identities.

## Administrator installation

Use the binary from the `el9` CI job artifact (built and tested in Rocky 9). Example service unit and sudoers rule are in [`deploy/`](deploy). The broker refuses root: run it as a `chaperone` system user with a client keytab for a scoped IPA role, and Slurm coordinator of the persona accounts (not `AdminLevel=Operator`, which can cancel any job and change nodes). On a single login node it listens on `127.0.0.1` only. Site installation (Ansible, preflight, revert, IPA role) lives in the site repository, not here. Pilot blast radius and test order: [`PILOT.md`](PILOT.md).

## Deployment gates

- **Tested:** policy/group logic, broker request handling and fail-closed `ensure` (unit, fake IPA/Slurm); full launch with two real UIDs, real sudo and ACLs incl. `0600` credential handoff, isolation and refusals (`tests/e2e`); real FreeIPA/Slurm provisioning, drift and collisions (`tests/integration`).
- **Not yet implemented:** disable/delete of dormant or ineligible personas; revocation of running jobs; partition/network enforcement (use Slurm account limits, `DenyAccounts`, firewall).
- **Shared-state caveat:** an AI persona can edit hooks/MCP settings later executed by its human, and can race the human's ACL repair with a symlink to grant itself the human's other files. Both are inside the accepted trusted-human risk; root never performs this repair. Concurrent human and persona sessions can still see each other's fresh `0600` files masked until the next launch/exit. `~/.claude.json` (a file in `HOME`) is not shared.
- **Credential caveat:** environment variables (including API keys) do not cross sudo; file-based harness logins do.
- **No automatic data firewall:** the persona reaches whatever its groups and world bits allow. It needs `--x` on the human's HOME, so world-readable files under HOME become readable to it. Audit protected data permissions on every filesystem before production use.

See [PLAN.md](PLAN.md) for the larger design and remaining requirements.
