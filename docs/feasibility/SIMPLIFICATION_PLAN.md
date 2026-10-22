# Group-only Chaperone: simplification plan

This plan is conditional. It applies only if the maintainer accepts
relaxations R1–R6 from [FEASIBILITY.md](FEASIBILITY.md#verdict). It also
changes postulates 4, 5 and 6, which is a product decision, not a side
effect of a PR:

- **P4:** the persona can read every other-readable file in its human's
  HOME by name (R5).
- **P5:** Claude needs a one-time `setup-token` (R1).
- **P6:** revocation reaches running work only by kill or `scancel` (R6).

Nothing here is implemented yet.

## Model

- **Identity:** `<user>-<profile>`, as today. One profile per security
  boundary; per-project profiles only where projects must stay apart (R2).
- **One group per persona** (as today): the human is a member. It is the
  persona's primary group, HOME's traverse group and the share group, all
  at once.
- **HOME:** `chgrp <persona-group> ~ && chmod 0710 ~`, done once by the
  human on first launch. Owner and contents are unchanged; no root needed.
- **Groups:** the persona gets the groups its profile approves. They are
  reconciled by the broker on each launch, never banned ones (checked
  including indirect FreeIPA membership). Existing project groups are
  reused. A folder used by several projects gets a resource group with the
  project groups nested in it.
- **Launch:** `_enter` checks identity, sets umask 007, and runs the login
  shell or the command as the persona with `HOME` = the human's HOME. No
  mounts, no ACLs, no walk.
- **Slurm:** unchanged. The persona is an ordinary SSSD user with its
  association; jobs see the same groups and HOME as the login node (T11).
- **Claude:** the human runs `claude setup-token` once and stores the token
  in a file only they write, `0640 <persona-group>`. `.bashrc` exports
  `CLAUDE_CODE_OAUTH_TOKEN` from it. `CLAUDE_CONFIG_DIR=~/.claude` keeps
  `.claude.json` out of the non-writable HOME. **Codex:** include `~/.codex`
  once, and again after a re-login.

## CLI

```sh
chaperone [ -p PROFILE ] [ CMD ARGS… ]   # unchanged; PATH lookup after the switch
chaperone include PATH       # chgrp <persona-group> PATH; dirs g+rwxs, files g+rw; no walk
chaperone include -R PATH    # preview count, refuse over budget or on foreign owner,
                             # banned group, mount or symlink escape; journal; then walk
chaperone exclude PATH       # undo from the journal (or g-rwx, back to the human's group)
chaperone access             # list journaled includes
```

All of these run as the human, unprivileged. A human can only `chgrp` their
own files to groups they are in, so `include` gains no rights. The journal
lives in the human's state directory.

## Retain, remove, add

| | Component | Change |
|---|---|---|
| keep | `broker.go` | persona, persona group and association; **plus** reconciling approved project-group memberships |
| keep | `enter.go` | identity checks, privilege drop, launch log; add umask 007; drop share and repair calls |
| keep | `policy.go` | profiles = approved groups, Slurm account, `include -R` budget |
| keep | `uninstall.go` | retire by default; purge chowns persona files back in journaled roots; ACL code goes |
| remove | `acl.go` + tests (~680 lines) | no ACLs |
| remove | bindfs view, mount namespace, session code (PR #13) | no mounts |
| remove | grant record, repair on launch, `walk_limit` | no implicit walks |
| remove | `setfacl` dependency, compute-node bindfs plan | |
| add | `include`/`exclude`/`access` (~150 lines + tests) | journaled `chgrp`/`chmod` |

Net: roughly −900 Go lines, no new dependency, and nothing on compute nodes.

**Least-privilege change.** The broker now edits members of *project* groups,
not only persona groups. Its FreeIPA permission must exclude banned and
administrative groups by filter. PR #13 already does this
(`permission_mod --ban`); the T06 tests must cover it against real FreeIPA.

## Migration and rollback

- **From the ACL build:** run the existing uninstall purge, which removes
  ACL entries from recorded trees, then install.
- **From the bindfs build:** stop the sessions (the views unmount), then
  install.
- **Rollback:** `chaperone exclude` for every journal entry, restore HOME's
  group and mode (recorded at first launch), and reinstall the old build.
  Persona-owned files stay persona-owned (T12.6). Purge chowns them back.

## Tests required for the implementation

- **Unit:** `include` refusals (foreign owner, banned group, HOME,
  credential dirs, mount, symlink escape, budget), journal round trip, and
  `exclude`.
- **e2e (CI):** turn `tests/feasibility/run.sh` T01–T05, T11 and T15 into
  assertions on the real binary; keep the FAIL findings as documented
  expectations.
- **Integration (disposable FreeIPA):** project-group reconcile, ban filter
  including nested groups (T06.6), and retire/purge.
- **Site pilot:** SSSD on compute nodes, several node types, Lmod, and a
  real `setup-token` login as the persona.

## Operational burden

**Less:** no FUSE or bindfs package, no per-session daemons, no compute-node
changes, no ACL repair, and Slurm jobs from HOME work.

**New:**
- humans need umask 002 for shared trees they write into;
- `include -R` on large trees costs about 3 MDS operations per entry;
- one Claude token step per human;
- persona lifecycle grows with per-project profiles.
