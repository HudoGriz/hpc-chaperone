# Chaperone — implementation plan

**Status:** design only; not yet implemented or deployed.  
**Objective:** minimal custom code, maximum reuse of the existing Linux/FreeIPA/Slurm infrastructure.

## Design contract

1. **Automatic identities:** Chaperone itself creates, reconciles, disables and eventually deletes a FreeIPA identity for each `(human, persona)` on first use. No manual per-user enrollment or pre-provisioning.
2. **Real identity:** `<user>-ai` by default; `<user>-ai-special` for an administrator-defined special profile. Each is a distinct numeric UID visible to SSSD on all nodes and a normal Slurm principal.
3. **Shared experience:** The persona uses the human's existing `HOME`, *writable* harness configs, authentication, skills, MCP setup, memories/history and projects. No copied config tree, second login or persona-specific memory.
4. **Restricted permissions:** The persona does **not** automatically inherit the human's groups. Only explicitly allowed existing group memberships and admin-approved persona groups are assigned. System filesystem, Slurm and network permissions enforce access.
5. **Generic runner:** `chaperone [options] [--] COMMAND [ARGS...]` starts any command, including Claude, Codex, OpenCode, Nextflow, `sbatch`, shells and other programs. Arguments are passed as an argv array without re-parsing them in a shell.
6. **Trusted users:** Running a harness without Chaperone is permitted and the human's responsibility; optional monitoring is informational. The site accepts risks introduced by fully shared writable state.
7. **Independent project:** Chaperone is site-wide HPC infrastructure, not part of AutoResearch HPC (ARH). ARH can be a pilot workload.

## CLI

```sh
chaperone claude
chaperone codex exec "run analysis"                # default profile: ai
chaperone -p ai-special claude
chaperone -p ai-special sbatch pipeline.sh
chaperone --list-personas
chaperone --status
chaperone -n -p ai-special -- codex exec "dry run"
```

The first non-option or `--` ends Chaperone option parsing. With no command, open an interactive persona shell. A missing or ineligible profile is an error; failure to enter the persona must **never** execute the command as the human instead.

## Administrator-owned profile configuration

Use one simple TOML file, e.g. `/etc/chaperone/policy.toml`, deployed by Ansible. **This is a proposed schema**, not an implemented parser.

```toml
schema_version = 1

[global]
default_persona = "ai"
username_template = "{user}-{persona}"
provision_on_first_use = true
shared_home = true
shared_harness_state = "read_write"
fail_closed = true

[global.eligibility]
any_human_group = ["research"]

[global.groups]
deny = ["restricted", "admins_restricted", "nas_*", "wheel"]

[personas.ai]
description = "Default restricted persona"

[personas.ai.groups]
keep = ["research", "hpc-users"]
add = ["agents"]

[personas.ai.slurm]
account = "agents"
partitions = ["all", "cpu", "gpu", "lab", "debug", "long"]
qos = ["normal", "long"]

[personas.ai.network]
policy = "standard"

[personas.ai-special]
description = "Specialized persona with additional approved research access"

[personas.ai-special.groups]
keep = ["research", "hpc-users", "genome-project"]
add = ["agents"]

[personas.ai-special.slurm]
account = "agents-special"
partitions = ["all", "cpu", "gpu", "lab", "debug", "long"]
qos = ["normal", "long"]

[personas.ai-special.network]
policy = "standard"

[network_policies.standard]
proxy = "site-proxy"
```

The names of site groups, accounts, partitions and policies above are **examples to validate against the cluster**. Additional profiles require only another `[personas.<profile>]` block.

**Group calculation:**

```text
persona_groups =
    (human_groups INTERSECT profile.groups.keep)
    UNION admin_approved_profile_groups
    MINUS global_denied_groups
    MINUS profile_denied_groups
```

- `keep` cannot grant a group the human does not have. `add` is admin-only and must be validated against a site approval list.
- Global restrictions override normal profile settings. Validate primary GID, nested FreeIPA groups and SSSD-resolved effective groups, not just configured supplementary groups.
- A user's optional configuration may select convenience defaults but never expand security permissions or invent identities.
- Profile names are validated, unique and immutable identifiers; renaming a profile is a lifecycle migration, not silently adding a second account.

## Minimal architecture

```text
user: chaperone -p ai-special codex ...
  └─ unprivileged CLI
     ├─ reads root-owned profile policy
     ├─ checks invoking identity/eligibility
     ├─ asks authenticated chaperone-provisiond on management host
     │  ├─ FreeIPA API on existing separate VM: create/reconcile persona + groups
     │  └─ Slurm accounting: create/reconcile persona association, limits, QOS
     ├─ confirms SSSD and allowed shared-home permissions
     └─ constrained sudo identity switch → exec codex with original argv
          └─ sbatch / srun / Apptainer run as persona UID on compute nodes
```

**Build only the indispensable pieces:**

- `chaperone`: small CLI; profile selection, on-demand provisioning request, environment/argv forwarding, dry-run/status.
- `chaperone-provisiond`: narrowly scoped, authenticated and audited broker for FreeIPA/Slurm provisioning and lifecycle reconciliation. No IPA admin credentials on login/compute nodes.
- **Only if required:** a tiny `chaperone-enter` helper when generated sudoers rules cannot safely perform the identity switch and shared-home preparation.
- Config/testing/deployment: Ansible and small scenario tests.

**Reuse instead of rebuilding:** FreeIPA API + SSSD, Slurm + MUNGE + `sacctmgr`, sudo, Unix permissions/ACLs, nftables + Squid, auditd, Prometheus/process-exporter and Goss. Avoid a custom proxy, per-node agent daemon, general-purpose sandbox engine, Slurm-prolog-based identity provisioning, and a duplicate user database.

## Provisioning and revocation

1. Resolve the **actual invoking human**; authorize requested profile against root-owned policy.
2. Authenticate the request to the provisioning broker (MUNGE is a candidate; validate freshness, binding and replay protection).
3. Lock `(human, profile)`; idempotently create or reconcile the FreeIPA user on the remote VM, with the human's `homeDirectory`, exact group set, no password/direct SSH, and consistent numeric UID.
4. Create/reconcile Slurm account associations and allowed QOS/partition policy. Verify effective Slurm restrictions; Unix groups are **not** Slurm accounts.
5. Wait for bounded SSSD identity visibility and check effective UID/GIDs on login/compute nodes.
6. Verify shared harness state and workspace access; launch as persona through a narrowly scoped sudo rule/helper. **Fail closed**, never fall back to the human UID.
7. Reconcile policy changes and changes to human membership automatically. When access is revoked, disable new sessions, stop/cancel affected running sessions/jobs according to site policy, invalidate caches as needed, and delete the persona after an audited retention window. Never delete the human's shared home, and avoid unsafe UID reuse.

The provisioning broker is a high-value privileged service: scope and test FreeIPA delegation, protect the service keytab and Slurm capabilities, validate all requested identities server-side, and audit every change.

## Enforcement boundaries

- **Filesystem:** use positive permission grants for approved research/home paths and remove inappropriate source-side world access to protected data. A POSIX `g:agents:---` entry is *not* a universal deny rule: other matching entries can still grant access. Test CephFS, CIFS and NFSv4 semantics separately.
- **Shared state:** same `HOME` and writable `~/.claude`, `~/.codex` etc. Test restrictive modes, default ACLs, atomic replacements, SQLite locks and simultaneous human/persona writes. The persona's `homeDirectory` in IPA must match `HOME`.
- **Credentials:** sharing authentication/state may expose service credentials or sensitive content already stored there. Non-shared private keys, diagnostic roots, administrative sockets and other credentials must remain inaccessible; the trust model explicitly accepts the residual shared-state risk.
- **Slurm:** work stays under the persona UID; apply accounts/QOS/partition policy explicitly. Compute nodes independently resolve persona groups. Check partition `AllowAccounts`/`DenyAccounts` semantics and accounting inheritance.
- **Networking:** enforce per-persona UID rules (`nftables meta skuid` or another tested kernel identity mechanism); do not assume `skgid` recognizes supplementary `agents` membership. Proxy environment variables alone are not enforcement.
- **Observability:** auditd/process-exporter/Prometheus/Goss for visibility and verification, not a guarantee that unchaperoned agent execution is impossible.
- **Privilege boundary:** blacklist neither command names nor interpreter paths as the sole security control; protect the underlying resources and privileged APIs.

## Implement in this order

1. **Proof-of-concept:** two profiles for one human, same writable harness state, different allowed group sets, denied clinical paths, one end-to-end Slurm/Apptainer workflow. Manual commands are acceptable *only for this disposable proof*, not for the product.
2. **Automatic identity lifecycle:** first-run FreeIPA/Slurm creation, idempotence, concurrent launch handling, policy reconciliation and cleanup.
3. **CLI and policies:** `chaperone -p <profile>`, strict TOML validation, shared-home permission repairs, exit codes/signals, sudo privilege transition.
4. **Enforcement validation:** protected roots on every node/filesystem, correct Slurm partitions/QOS, restricted sockets and optional network allowlist.
5. **Deployment:** minimal Ansible role plus audit/monitoring and cross-node integration tests. Pilot with a real research workflow.

**Release gate:** both `ai` and `ai-special` can run the same existing harness with shared writable state, but only access their configured resources on login nodes, Slurm jobs and within Apptainer; first-run provisioning and revocation require no per-user administrative intervention.

## Explicit non-goals

- Preventing a trusted human from choosing unchaperoned execution.
- Preventing the human from later executing agent-modified scripts/hooks in their unrestricted context.
- Creating isolated copies of the human's harness authentication, memory or config.
- Replacing FreeIPA, Slurm, Linux permissions, or deploying a separate container scheduler.
- Claiming security against administrators/kernel compromise or disclosure of data the persona is intentionally authorized to read.

The overarching rule remains: **Chaperone configures; existing identity, filesystem, scheduler and network mechanisms enforce.**
