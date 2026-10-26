# Operating Chaperone on a production HPC

Site specifics (hosts, group names, limits) belong in the site's deployment
repository, not here.

## What each step can break

| Step | Touches | Worst case | Undo |
| --- | --- | --- | --- |
| Login node play, first part | service user, binary, policy, state directories | nothing launches yet | `uninstall.yml` |
| FreeIPA play (`deploy/ipa.py`) | eligibility group and its members, service, permissions, privilege, role | additive, except members: with `chaperone_users` set, others leave the group | `uninstall.yml` |
| Slurm play (`deploy/slurm.sh`) | persona accounts, their `slurm_limits`, service user as their coordinator | agents compete for compute like any user unless limits are set | `uninstall.yml` |
| Login node play, last part | keytab, sudo rule | a broken sudo rule breaks sudo on the node; it is validated with `visudo -cf` | `uninstall.yml` |
| First launch of a profile | persona, its group and the human's traverse group `<user>-chaperone` in FreeIPA, the persona's Slurm association; the persona in the human's groups minus banned ones; HOME and each startup file get the traverse group, group bits `--x` and `r--` (old group and mode kept in `/var/lib/chaperone/home`); `~/.chaperone/<persona>` is created, the persona's HOME | personas traverse HOME and read the startup files | `uninstall.yml` (gives HOME back; purge removes `~/.chaperone`) |

Slurm controller, slurmdbd and munged configuration are not changed. Persona
accounts only restrict submission if `AccountingStorageEnforce` includes
`associations`; check it before the pilot.

## What a persona can damage

- **HOME**: its own HOME; in the human's HOME, what the human included
  and what others may.
- **Its groups' data**: the human's groups minus `ban_groups`, read and
  written as those groups allow. Ban every group whose data an agent must
  not touch.
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
  - write `member` on every group except those in any profile's
    `ban_groups` (it puts personas into their human's groups and the human
    into persona groups). Ban `admins` and every other privileged group:
    the right covers any user, not only personas;
  - write `description` on users whose userclass starts with `chaperone:`
    (personas only; `user-add --noprivate` rewrites it).

  The host principal of the login node may fetch the broker keytab.
- **Slurm**: coordinator of the persona accounts (`deploy/slurm.sh`), never
  `AdminLevel=Operator`. The service user needs the same UID on the slurmdbd
  host (MUNGE authenticates by UID). If `AccountingStorageHost` points at
  `localhost` (configless Slurm), set `chaperone_slurmdbd_host` so the broker
  gets its own `SLURM_CONF`.

Residual risk: FreeIPA does not enforce unique IDs, the role may create
any user, and it may change members of every group no profile bans. Only the broker keytab (`0400`, service user) carries the role, and
`_provision` runs only as a child of `_enter` with arguments root chose: there
is no daemon or socket to attack. Taking the keytab needs root on the login
node. `_enter` still refuses a persona whose UID does not map back to it.

## Sessions

Each launch is a private mount namespace, which only marks the session:
when the command exits, the persona's processes left in it are killed.
Nothing is mounted. `chaperone include -R` walks its tree once, as the
human; on CephFS each entry costs an open, a chown and a chmod on the MDS
(budget: 100,000 entries). `chaperone seed` is a tar as the human into a
tar as the persona.

## Upgrading from an earlier build

The persona's HOME moved from the human's HOME to
`~/.chaperone/<persona>`; the broker refuses a persona whose FreeIPA HOME
differs. Purge with the old build's `uninstall.yml` first (it gives HOME
and startup files back and deletes the old personas), then install the new
build. `chaperone exclude` every include before the purge. Site variables:
drop `bind`, `bind_ro`, `user_bind` (and from ACL builds `walk_limit`,
`workdir`, `write`, `read`, `home_readonly`, `writable`, `hidden`); add
`harnesses`. bindfs is no longer used.

## Test order (each step needs the operator's go)

1. Make CI green, merge, and take the `el9` artifact.
2. Run `install.yml` with one **test** account in `chaperone_users` that
   holds a restricted group, so the pilot proves the persona does not get it.
3. Check the system log (`journalctl -t chaperone`).
4. As the test account, for each profile:
   - `chaperone -- id`, `chaperone seed`, a harness as the persona;
   - banned groups' data is refused, allowed groups' data is readable;
   - the persona's HOME is its own; it cannot write the human's harness state;
   - `sbatch` runs as `<user>-<profile>` on every node type;
   - the agent login works, and exit codes and Ctrl-C pass through.
5. Run `uninstall.yml`, check that nothing is left, then reinstall.

## Kill switch and revocation

- **Stop all launches**: delete `/etc/sudoers.d/chaperone`. Running
  sessions and jobs continue.
- **Revoke one user**: remove them from `chaperone_users` and re-run
  `install.yml` (or `ipa group-remove-member`). Their next launch is refused.
  Then end what already runs: `scancel -u <user>-<profile>` and, on the
  login nodes, `pkill -u <user>-<profile>` (sessions end with their command).
- **Persona files outside HOME** (scratch, `/tmp`, job output elsewhere)
  stay with their (retired, named) persona. Only after a purge do they keep
  a bare UID: find them per filesystem with `find <fs> -xdev -uid <uid>`;
  doing that automatically on a shared filesystem would be too costly.
- For everything else, run uninstall: retire by default, purge with
  `-e chaperone_purge=true`.
