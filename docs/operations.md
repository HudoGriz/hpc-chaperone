# Operating Chaperone on a production HPC

Site specifics (hosts, group names, limits) belong in the site's deployment
repository, not here.

## What each step can break

| Step | Touches | Worst case | Undo |
| --- | --- | --- | --- |
| FreeIPA play (`deploy/ipa.py`) | eligibility group, service, permission, privilege, role | additive; the broker can create users and groups | `uninstall.yml` |
| Slurm play (`deploy/slurm.sh`) | persona accounts, service user as their coordinator | agents compete for compute like any user (limits are a site choice) | `uninstall.yml` |
| Login node play | service user, binary, unit, cert, sudo rule | a broken sudo rule breaks sudo on the node; validate with `visudo -cf` | `uninstall.yml` |
| First launch | persona and its group in FreeIPA and Slurm, ACLs on agent dirs and the launch dir | ACLs only add entries; the first launch in a tree walks it (metadata load) | `chaperone _uninstall` |

Slurm controller, slurmdbd and munged configuration are not changed.

## What a persona can damage

- **Shared agent dirs** (`~/.claude`, `~/.codex`, …): full read-write.
- **Its launch directory**: full read-write on the whole tree. Don't launch
  it from a tree holding restricted data.
- **Anything world-writable**, like any user.
- **Slurm**: only its account, within that account's limits.

It has no password, no SSH key and a `nologin` shell. Its only way in is
`sudo chaperone _enter`.

## Broker privileges

- **FreeIPA** (`deploy/ipa.py`):
  - `System: Add Users`;
  - `System: Add User to default group`;
  - `System: Read UPG Definition` (`user-add` fails without it);
  - `System: Add Groups`;
  - write `member` on groups whose description starts with `chaperone:`
    (persona groups only);
  - write `description` on users whose userclass starts with `chaperone:`
    (personas only; `user-add --noprivate` rewrites it).

  The host principal of the broker host may fetch the broker keytab.
- **Slurm**: coordinator of the persona accounts (`deploy/slurm.sh`), never
  `AdminLevel=Operator`. The service user needs the same UID on the slurmdbd
  host (MUNGE authenticates by UID). If `AccountingStorageHost` points at
  `localhost` (configless Slurm), give the broker its own `SLURM_CONF`.
- **TLS**: a loopback-only certificate, name-constrained to
  `localhost`/`127.0.0.1`, so a stolen key cannot impersonate another host.

Residual risk: a compromised broker could create a user or group whose GID
equals an existing group's GID. FreeIPA does not enforce unique GIDs.

## Lessons from the first pilot

- Rocky 9's munge 0.5.13 has no `unmunge -N`; the output parser handles both forms.
- The FreeIPA role needs `System: Read UPG Definition`. CI now runs the
  broker with exactly the role from `deploy/ipa.py`, so a missing right fails
  CI instead of the pilot.
- SSSD caches identities (90 min by default). `_enter` drops the human's and
  the persona's cached entries on every launch.
- sudo's `env_reset` drops the site proxy from `/etc/profile.d`;
  `deploy/chaperone.sudoers` keeps proxy, locale and agent variables.
- Claude Code keeps `~/.claude.json` in HOME's root, which the persona
  cannot write. The persona uses `CLAUDE_CONFIG_DIR=~/.claude`, seeded once
  from the human's file, so it never re-onboards.

## Test order (each step needs the operator's go)

1. Make CI green, merge, and take the `el9` artifact.
2. Run `install.yml` with one **test** account in `chaperone_users` that
   holds a restricted group, so the pilot proves the persona does not get it.
3. Check the broker log (`journalctl -u chaperone-provisiond`).
4. As the test account:
   - `chaperone --status` and `chaperone -- id`;
   - restricted data and the human's private files are refused;
   - `sbatch` runs as `<user>-ai` on every node type;
   - the agent login works, and exit codes and Ctrl-C pass through.
5. Run `uninstall.yml`, check that nothing is left, then reinstall.

## Kill switch

- Delete the sudo rule: no new launches, and running sessions finish.
- Stop `chaperone-provisiond`: no new personas.
- For everything else, run the full uninstall.
