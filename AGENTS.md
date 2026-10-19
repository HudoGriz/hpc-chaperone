# Development guidance for Codex

Read `README.md` and `PLAN.md` before changing code. This repository is a **prototype**, not an approved production security boundary.

## Invariants

- `chaperone COMMAND` selects profile `ai`; `-p NAME` selects an administrator-defined persona. Never allow arbitrary target usernames supplied by clients.
- Chaperone must **automatically** create, reconcile, disable and eventually delete per-human FreeIPA/Slurm personas; humans do not manually enroll AI accounts.
- Human and persona intentionally use the **same writable** HOME/harness state, logins, skills and memories. Do not split these without an explicit product decision.
- Keep groups as an intersection with the human's memberships plus strictly approved site additions; global denials win. Never treat POSIX ACL named groups as unconditional deny rules.
- A failed provisioning/identity check must never fall back to launching under the human UID.
- Preserve native Slurm, subprocess, Apptainer and Nextflow execution as the persona.
- Unchaperoned execution by trusted humans is permitted; do not implement aggressive kill-on-detect logic.

## Coding and verification

- Stay small: Go standard library and existing site tools first; justify any new daemon/dependency.
- Use `exec.Command` argv, never concatenated shell commands for inputs from users, LDAP, or policies.
- Keep privileged policy rooted at `/etc/chaperone`, root-owned; CLI convenience preferences cannot increase privileges.
- Add unit tests for every group-resolution, identity, policy and provisioning change; run `make test`, `make lint`, `make build`.
- Run integration tests with disposable identities and test filesystems first. **Never** mutate production FreeIPA, Slurm, ACLs, firewall, sudoers or clinical data without explicit administrative approval.
- Be candid about unimplemented controls. CI tests do not constitute HPC security certification.

## Next engineering priorities

1. Test FreeIPA and Slurm command behavior using injectable command runners and staging services.
2. Implement automatic disable/deletion lifecycle and revocation of running jobs/sessions.
3. Validate shared-home/SQLite/harness state behavior for Claude Code, Codex and OpenCode.
4. Implement site network/Slurm enforcement and cross-node negative access tests.
5. Build a guarded Ansible installer with check/dry-run and rollback.
