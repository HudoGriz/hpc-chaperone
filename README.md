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

- One binary: unprivileged `chaperone` client, restricted root-only `_enter` subcommand, central `provisiond` HTTPS service.
- The client obtains a **MUNGE credential** binding its UID and requested profile and sends it over verified HTTPS to the broker.
- The broker decodes it with `unmunge` and derives the human via NSS; no caller-supplied username or account is trusted. It reconciles the FreeIPA user and groups with `ipa` and the Slurm association with `sacctmgr`.
- The client adds ACLs to approved shared-home state, then uses a tightly scoped sudo rule to launch `_enter`. `_enter` independently validates user eligibility, expected groups, and shared `HOME`, drops privileges, and execs the requested command.
- Compute-node jobs require no Chaperone daemon; they use normal Slurm and SSSD identities.

## Administrator installation (staging only)

1. Build `chaperone` with `make build` **on a compatible RHEL 9 system** (glibc/NSS linking matters).
2. Install it root-owned, mode `0755`, at **`/usr/local/bin/chaperone`** on login nodes and the management host.
3. Copy and adapt `config/policy.example.json` to `/etc/chaperone/policy.json`, owned by root and not group/world writable. Use real FreeIPA groups and Slurm accounts.
4. Configure a sudoers rule scoped to `_enter` only (example in `deploy/chaperone.sudoers`; adapt eligibility group). Never permit unrestricted root invocation of this binary.
5. Install MUNGE client and `setfacl`/`find` on login nodes, `unmunge`, FreeIPA `ipa` CLI, Slurm `sacctmgr`, a valid Kerberos service ticket/keytab, and an HTTPS certificate on the **management host**.
6. On the management host start `chaperone provisiond --listen 127.0.0.1:9443 --cert /etc/chaperone/tls.crt --key /etc/chaperone/tls.key`; bind to a reachable management interface only after network access restrictions are installed. Match `provision_url` and trust the certificate CA on login nodes.
7. Configure IPA delegation and Slurm coordinator privileges **once for the service**. Create the configured shared Slurm accounts/groups as approved site infrastructure. No individual persona provisioning is needed.
8. Test with `chaperone -n`, then a disposable test user and test groups/data before enabling real users.

The repo intentionally does **not** contain automated changes to FreeIPA/Slurm, SSH/HBAC, network firewalls, patient-data ACLs, or production Ansible inventory. Those must be designed and verified on your cluster. The sample commands are a staging guide, not a safe production installer.

## Deployment gates

- **Not yet implemented:** periodic cleanup/disable/delete of dormant/ineligible personas; complete profile-aware network/partition enforcement; full service deployment playbook; cross-node ACL and shared-state integration tests. First-launch creation and per-request group reconciliation are implemented but **not tested against a live FreeIPA or Slurm system**.
- **Shared-state caveat:** an AI persona can edit hooks/MCP settings later executed by its human. Shared harness auth may contain secrets. This is an explicit trusted-human risk, not an isolation guarantee.
- **ACL caveat:** currently grants access to configured existing harness directories using `setfacl`; chmod `0600`, atomic replacements, unusual filesystems and files owned by other UIDs need pilot testing. Human's group-readable or world-readable diagnostics also require a source-side permissions fix. No ACL denies data that is otherwise accessible through another matching group/permission.
- **Credential caveat:** the first implementation preserves file-based harness sessions but does not transparently forward every user environment variable (including API-key environment variables) across sudo. This needs a safe environment-passing design before treating all harnesses as identical.
- **No automatic data firewall:** deploying this binary alone provides **no verified protection** for world-readable or otherwise accessible clinical data. Validate protections independently and on every compute node before production use.

See [PLAN.md](PLAN.md) for the larger design and remaining requirements.
