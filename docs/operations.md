# Operating Chaperone on a production HPC

Site specifics (hosts, group names, limits) belong in the site's deployment
repository, not here.

## What each step can break

| Step | Touches | Worst case | Undo |
| --- | --- | --- | --- |
| Login node play, first part | service user, binary, policy, state directories | nothing launches yet | `uninstall.yml` |
| FreeIPA play (`deploy/ipa.py`) | service, permissions, privilege, role | existing site-managed eligibility groups are never changed | `uninstall.yml` |
| Slurm play (`deploy/slurm.sh`) | persona accounts, their `slurm_limits`, service user as their coordinator | agents compete for compute like any user unless limits are set | `uninstall.yml` |
| Login node play, last part | keytab, sudo rule | a broken sudo rule breaks sudo on the node; it is validated with `visudo -cf` | `uninstall.yml` |
| First launch | persona and its group in FreeIPA and Slurm, ACLs on the profile's paths and the launch dir | ACLs only add entries; the first launch in a tree walks it, up to 20 000 entries (metadata load) | `chaperone _uninstall` |

Slurm controller, slurmdbd and munged configuration are not changed. Persona
accounts only restrict submission if `AccountingStorageEnforce` includes
`associations`; check it before the pilot.

## What a persona can damage

- **Its write paths** (`~/.claude`, `~/.codex`, …): full read-write.
- **Its launch directory**: full read-write on the whole tree, `.git`
  included. Don't launch it from a tree holding restricted data.
- **Anything world-writable**, like any user.
- **Slurm**: only its account, within that account's limits.

It has no password, no SSH key, a `nologin` shell and `no_new_privs`: no
setuid program works for it. Its only way in is `sudo chaperone _enter`.

## Broker privileges

The broker is `chaperone _provision`, run by `_enter` for one launch as the
service user `chaperone`, never as root. There is no daemon.

- **FreeIPA** (`deploy/ipa.py`):
  - `System: Add Users`;
  - `System: Add User to default group`;
  - `System: Read UPG Definition` (`user-add` fails without it);
  - `System: Add Groups`;
  - write `member` on groups whose description starts with `chaperone:`
    (persona groups only);
  - write `description` on users whose userclass starts with `chaperone:`
    (personas only; `user-add --noprivate` rewrites it).

  The host principal of the login node may fetch the broker keytab.
- **Slurm**: coordinator of the persona accounts (`deploy/slurm.sh`), never
  `AdminLevel=Operator`. The service user needs the same UID on the slurmdbd
  host (MUNGE authenticates by UID). If `AccountingStorageHost` points at
  `localhost` (configless Slurm), set `chaperone_slurmdbd_host` so the broker
  gets its own `SLURM_CONF`.

Residual risk: FreeIPA does not enforce unique IDs, and the role may create
any user. Only the broker keytab (`0400`, service user) carries the role, and
`_provision` runs only as a child of `_enter` with arguments root chose: there
is no daemon or socket to attack. Taking the keytab needs root on the login
node. `_enter` still refuses a persona whose UID does not map back to it.

## The grant record

`/var/lib/chaperone/grants` (root, `0600`) lists every path a launch shared,
written before the sharing. Purge and policy revocation work from it, not
from the current policy. Retire keeps it for a later purge. Keep it when
changing or upgrading Chaperone.

## Upgrading from a build before profiles

Builds before per-profile policy kept their launch log elsewhere and ran a
broker daemon. Uninstall with the old build's `uninstall.yml`, check that
nothing is left, then install the new build.

## Test order (each step needs the operator's go)

1. Make CI green, merge, and take the `el9` artifact.
2. Add one **test** account holding a restricted group to the site-managed
   FreeIPA eligibility group, then run `install.yml`. Confirm the persona
   does not inherit the human's restricted group.
3. Check the system log (`journalctl -t chaperone`).
4. As the test account, for each profile:
   - `chaperone --list-personas` and `chaperone -- id`;
   - restricted data and the human's private files are refused;
   - `sbatch` runs as `<user>-<profile>` on every node type;
   - the agent login works, and exit codes and Ctrl-C pass through.
5. Run `uninstall.yml`, check that nothing is left, then reinstall.

## Kill switch and revocation

- **Stop all launches**: delete `/etc/sudoers.d/chaperone`. Running
  sessions and jobs continue.
- **Revoke one user**: remove them from the configured FreeIPA eligibility
  group (`ipa group-remove-member`). Their next launch is refused.
  Then end what already runs: `scancel -u <user>-<profile>` and
  `pkill -u <user>-<profile>` on the login nodes. Their ACL grants stay until
  uninstall.
- **Unshare one launch directory**: as root,
  `setfacl -R -x u:<persona>,d:u:<persona> DIR`, and
  `find DIR -user <persona> -exec chown -h <user>: {} +`.
- **Persona files outside the recorded trees** (scratch, `/tmp`, job
  output elsewhere) stay with their (retired, named) persona. Only after a
  purge do they keep a bare UID: find them per filesystem with
  `find <fs> -xdev -uid <uid>`; doing that automatically on a shared
  filesystem would be too costly.
- For everything else, run uninstall: retire by default, purge with
  `-e chaperone_purge=true`.

## Explicit per-profile access

`workdir` in each persona profile is `off` (default), `auto` (share a human-owned project directory if eligible), or `required` (abort if sharing fails). Filesystem errors while sharing an eligible directory are fatal in both modes. There is no internal credential-dotfile denylist; all HOME-relative read/write access is granted only by the root-owned profile. Do not include sensitive credential paths unless an administrator deliberately accepts the risk. `install` does not automatically re-enable disabled identities after retirement.

## FreeIPA teardown scoping

`ipa.py retire` and `ipa.py uninstall` require an explicit `--personas` list collected from the Chaperone grant records on all participating login nodes. This prevents accidental realm-wide disabling/deletion of other installations. Unrecorded identities (e.g., a provisioning attempt interrupted before grant recording) require administrative inspection. Do not claim a complete teardown merely because the playbook exits successfully; audit remaining marked objects per deployment.

Each broker host now owns separately named FreeIPA role, privilege and permission objects. Eligibility groups belong to site administrators and are never created, changed, or deleted by Chaperone. Existing unscoped legacy roles require manual migration review.

`PR_SET_NO_NEW_PRIVS` restricts descendants of the locally launched process, not independent Slurm daemon-launched worker tasks. Pilot tests must verify worker-side policy separately.

## Profile-defined launch environment

Persona shell (`shell`, `login`), environment forwarding
(`forward_env`), fixed variables (`env`) and one-time state initialization
(`seed_files`) are root-owned profile decisions, not built-in harness rules.
`install.yml` renders sudo's `env_keep` from the union of exact
`forward_env` names, and the launcher filters them again per persona.
Only `${HOME}` is interpolated in fixed profile variables. The example
explicitly opts into Claude's optional legacy config migration; without it,
Chaperone never copies harness state automatically.

## One source of configuration

`chaperone_policy.eligible_groups` is the sole source for runtime eligibility
and must name existing FreeIPA groups. The installer does not bootstrap or
modify those groups. The broker's POSIX UID must be explicitly set via
`chaperone_uid` after checking the site's UID registry; no silent fallback.
The example deploys only `ai`, with every optional access and harness setting
declared there. Extra persona profiles inherit only the default profile's
shell when not specified, never its filesystem permissions or credentials.
