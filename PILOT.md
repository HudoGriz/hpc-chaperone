# Pilot on a production HPC: blast radius and test order

Site specifics (hosts, group names, limits) belong in the site repository's
runbook, not here. Never commit credentials: `make secrets` (gitleaks) runs in
CI and in `.githooks/pre-push` (`git config core.hooksPath .githooks`).

## What each deployment step can break

| Step | Touches | Worst case | Undo |
| --- | --- | --- | --- |
| Preflight | reads site facts, one throwaway ACL probe dir in `/home` | a few filesystem metadata ops | none needed |
| IPA objects | new group, service, permission, privilege, role | additive; a too-wide permission (verify it in disposable FreeIPA first) | `ipa *-del` |
| Slurm account | persona account, `chaperone` as its coordinator | without limits, agents can fill the cluster | `sacctmgr delete` |
| Install on login node | local service user, binary, unit, cert, sudoers | broken sudoers breaks sudo on the node; `visudo -cf` blocks it | revert playbook |
| First launch | persona in IPA + Slurm, ACLs on shared agent dirs | ACLs only add entries; first launch walks the shared dirs (metadata load) | manual persona delete |

Slurm controller, slurmdbd and munged configuration are not changed.

## What a persona can damage

- Shared agent dirs (`~/.claude`, `~/.codex`, `~/.config/opencode`): full rwx.
- Anything writable by its `keep_groups`: audit group-writable project dirs.
- Anything world-readable below the human's HOME: readable (persona has `--x` on HOME).
- Rest of the human's HOME, other users, denied-group data: no access unless world bits or a kept group allow it.
- No login of its own: no password, no SSH key; only entry is `sudo _enter`.
- Slurm: only its account and the limits set on it. Its jobs load the shared filesystem like any user's.

## Least privilege (fixed before the pilot)

- Broker is a Slurm **coordinator** of the persona accounts, not
  `AdminLevel=Operator` (Operator can cancel any job, change nodes and
  partitions). Integration CI runs sacctmgr as that coordinator and checks it
  cannot add users outside its accounts.
- The self-signed broker cert is `CA:TRUE` in the system trust store, so it is
  name-constrained to `localhost`/`127.0.0.1`: a stolen key cannot impersonate
  other hosts. e2e CI uses the same cert form.

## Test order (each step needs operator go)

1. CI green, merge, take the `el9` artifact.
2. Preflight; review its output.
3. Pilot-only eligible group with one **test** account: no restricted-data groups, fresh HOME.
4. Persona account with tiny limits (e.g. `GrpTRES=cpu=8`, `MaxJobs=5`), denied on high-priority partitions.
5. Install with `--check --diff`, then for real.
6. As the test account:
   - `chaperone --status`, `chaperone -- id`
   - reading restricted data and a denied-group file is refused
   - `sbatch` runs as `<user>-ai` on every compute node type (SSSD resolves the persona)
   - shared agent login works; exit codes and Ctrl-C pass through
7. Rollback drill: revert playbook, confirm sudo rule is gone, reinstall.
8. First real user, after `find ~ -perm -o=r -type f` (exposure) and
   `find ~/.claude ~/.codex | wc -l` (ACL walk size). Watch metadata-server load.

## Kill switch

Delete `/etc/sudoers.d/chaperone`: no new launches, running sessions finish.
Stopping `chaperone-provisiond` stops new persona creation. Personas are
removed manually: `scancel -u <persona>; sacctmgr -i delete user <persona>; ipa user-del <persona>`.
