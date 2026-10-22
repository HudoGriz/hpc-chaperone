# Chaperone: hybrid design (groups everywhere, `bind` on login nodes)

Status: **design, not implemented, not yet prototyped.** It follows from
[FEASIBILITY.md](FEASIBILITY.md):

- groups alone cannot share harness state between two UIDs (F1);
- a mount mapping can share it, for every harness alike;
- only the login node needs that mapping, because Slurm jobs only need data,
  and groups already give jobs the same data rights as the login node (T11).

It keeps relaxations R3–R6 of the verdict and drops R1 (no `setup-token`
needed). Plan requirement 5 ("no mounts") is dropped **for login nodes
only**. Compute nodes and Slurm stay untouched.

## Phase 1: one persona per human

One profile, `ai`, so one persona `<user>-ai` per human. Projects are left
for later (see [Future](#future-separate-projects)).

| | Login node (interactive agent) | Compute node (Slurm job) |
|---|---|---|
| Identity | `<user>-ai` | `<user>-ai` |
| Project data | FreeIPA groups | the same groups, the same rights |
| HOME | `0710`, group = persona group | the same: jobs from HOME work |
| Harness state | `bind`: bindfs mapping, writes land as the human | not visible |
| Mounts | private mount namespace per session | none |

- **Identity:** created automatically on first launch. Its groups are the
  profile's approved groups, never banned ones (checked including indirect
  FreeIPA membership), reconciled by the broker on each launch.
- **One group per persona:** the human is a member. It is the persona's
  primary group and HOME's traverse group.

### CLI

```sh
chaperone [CMD ARGS…]            # launch; PATH lookup after the identity switch
chaperone bind [--ro] PATH       # map a directory in HOME into the persona's sessions
chaperone unbind PATH
chaperone include [-R] PATH      # group sharing; works on every node
chaperone exclude PATH
chaperone access                 # binds, includes and groups
```

All of these run as the human, unprivileged. `bind` and `include` record
the request; `_enter` applies binds at the next launch.

### `bind`: how it works

On the login node, `_enter` (root) runs these steps for each session:

1. Unshare a mount namespace and make `/` a recursive slave, so nothing
   propagates back to the host.
2. Bind the real HOME to a private source path, then mount
   `bindfs --map=human/persona:@human/@persona --enable-lock-forwarding
   --multithreaded -o allow_other,default_permissions` from it onto a
   hidden view. Mounting bindfs over its own source deadlocks, as the lab
   pilot showed.
3. For each bound directory, bind-mount `view/PATH` over `HOME/PATH`;
   `--ro` remounts it read-only. **Directories only.** `rename()` onto a
   mount point fails with `EBUSY`, and harnesses replace their state files
   by rename. Inside a bound directory, renames work.
4. Detach the view and the source (`umount2(MNT_DETACH)`). Only the bound
   directories stay reachable through the mapping. Symlinks inside them
   resolve outside the mapping, so they get the persona's own rights.
5. Drop to the persona (no_new_privs) and run the command. When the
   namespace ends the mounts go, and `_enter` stops its bindfs.

Each session gets its own namespace and bindfs, so parallel sessions are
independent. Locks are forwarded, so human, persona and a second session
lock the same files correctly; the lab pilot showed this with flock, fcntl
and SQLite.

### Who may bind what

- **Profile defaults**, for example `.claude`, `.codex`, `.config/opencode`.
- **The human may bind more** with `chaperone bind`, unless the profile
  sets `"user_bind": false`.
- **Banned groups refuse a bind.** A bind is refused if the directory, or
  any file or directory below it, belongs to a banned group. The reason:
  the mapping makes the persona the owner of the human's files, and owner
  bits ignore the group, so a banned-group file inside a bind would be
  readable.
- **How the ban is checked:**
  - when the bind is recorded, `chaperone bind` walks that one tree
    (`find -P -xdev`, symlinks not followed, refusing on a mount inside, a
    foreign owner or a banned group);
  - profile defaults are checked the same way when first activated;
  - a tree larger than the profile's walk budget is refused;
  - this is the only walk, and it is explicit.
- **Not re-checked at launch** (no walks on launch). A banned-group file the
  human moves into a bound directory later becomes readable. That is the
  same class as F4: the ban stops the persona, not the human's own
  `chgrp`/`cp`.
- There is no built-in deny list beyond banned groups. What else may be
  bound is the profile author's decision.

**Harness-agnostic.** A bound directory behaves for the persona exactly as
it does for the human: same owner, same 0600 files, atomic renames and
locks. No harness is named in code.

**Trust boundary.** A read-write bound directory carries the human's
authority over it. Hooks in `settings.json` are the obvious example. Bind
those `--ro` or don't bind them.

### Deferred

- **`~/.claude.json`** (state file directly in HOME): ignored for now. It
  cannot be bound, and HOME is not writable for its temp file. Later
  options:
  - a profile `env` redirect (`CLAUDE_CONFIG_DIR=~/.claude`);
  - accepting that the persona keeps its own copy.
- **Writes only in the launch directory** (proposed, not decided): make the
  session's mount tree read-only (`mount_setattr(AT_RECURSIVE,
  MOUNT_ATTR_RDONLY)`, EL9 has it), then reopen:
  - the launch directory;
  - the bound directories;
  - a private `/tmp`.

  It holds on login nodes only. In Slurm jobs the persona's groups decide.

## Decided

- **No bind of HOME as a whole**, not even read-only. It would give the
  persona:
  - the human's credentials (SSH keys mean logging in as the human,
    outside Chaperone);
  - owner rights over human-owned files of banned groups;
  - a different view on login nodes than in jobs;
  - FUSE cost on every HOME access.

  Only listed directories are bound.
- **One persona per human for now.** Projects come later.
- **Humans may bind beyond the defaults**, except trees that contain banned
  groups.

## Future: separate projects

**Open; both options are kept.** The kernel separates only by UID: one
persona's groups always add up (`sg` and Slurm restore them, F2), and Slurm
jobs ignore the login node's mounts.

### Option A: hard separation, one persona per project

- A **project profile template** in the policy: `chaperone -p @projA`
  becomes `alice-projA`, with exactly one approved group, `projA`.
- It is allowed if the human is in `projA` and `projA` is not banned.
- It is created on first use and then reused; there is no admin work per
  project.
- A launch with `@projA` can never reach `projB`, on the login node or in
  any job.
- **Same agent, separate memory:** every project persona binds the same
  harness directories (one login, same settings and skills). A
  per-project directory is bound over each harness's history:

```json
"templates": {
  "@": {
    "groups": ["{project}"],
    "bind": [".claude", ".chaperone/{project}/claude-projects:.claude/projects",
             ".codex",  ".chaperone/{project}/codex-sessions:.codex/sessions"]
  }
}
```

- **Costs:** one FreeIPA user and Slurm association per (human, project).
  Names must fit the 32-character UID limit, so long project names need a
  rule.

### Option B: soft separation, one persona with the others hidden

- The persona keeps all approved groups. A session for projA hides the
  other projects' paths (`tmpfs` over them) and starts in projA.
- This prevents accidental mixing only. **It is not a security boundary:**
  the persona holds the groups, and any Slurm job it submits sees
  everything.

### Never covered by either (as F4–F6)

- world-readable data;
- `0644` files in the human's HOME that a persona can open by name;
- data the human copies across projects.

## Retain, remove, add (phase 1)

| | Component | Change |
|---|---|---|
| keep | `broker.go` | persona, group, association, approved-group reconcile |
| keep | `enter.go` | identity checks, privilege drop, log; **plus** namespace and `bind` steps 1–5 |
| keep | `policy.go` | profile, default binds, `user_bind`, walk budget |
| keep | `uninstall.go` | retire by default; purge chowns persona files back in journaled roots |
| reuse | PR #13 bindfs code | narrowed from all of HOME to the listed directories |
| remove | `acl.go` + tests (~680 lines), grant repair, `setfacl` | no ACLs anywhere |
| remove | bindfs over all of HOME, compute-node bindfs plan | |
| add | `bind`, `unbind`, `include`, `exclude`, `access` | journaled records, ban walk on `bind` |

**Least-privilege change.** The broker now edits members of approved
(project) groups. Its FreeIPA permission must exclude banned and
administrative groups by filter, as PR #13's `permission_mod --ban` does.

## Prototype tests, phase 1 (local container, `--device /dev/fuse --cap-add SYS_ADMIN`)

1. The real Claude and Codex binaries as human and persona, in parallel,
   through `bind` (login, rename, locks, owner stays the human).
2. **Leak check:** after step 4 no path in the session reaches the detached
   view or an unbound HOME file through the mapping (`/proc/*/root`,
   `/proc/*/cwd`, open fds, `/proc/self/mountinfo`, symlinks in bound
   directories).
3. **Ban refusal:** `bind` of a tree with a banned-group file or directory
   at any depth is refused before anything is recorded; a clean tree is
   accepted; the walk budget is enforced.
4. A Slurm job from HOME and from a project directory, with no mapping on
   the compute side.
5. Plus the T01–T15 checks from `tests/feasibility/run.sh`.

## Phase 1 implementation: deviations from this plan

Built on PR #13's bindfs code (merged into this branch).

1. **The view is not detached (step 4).** It stays mounted under
   `/run/chaperone` (root, `0700`), which the persona cannot enter; e2e
   checks this. Detaching would leave bindfs unable to unmount itself at
   the end of the session, for no gain.
2. **The ban walk runs in `chaperone bind`, as the human; profile defaults
   are not walked** (they are the policy author's). The binds are kept in
   `~/.config/chaperone/binds` and passed to `_enter` as `--bind`. Editing
   the file skips the walk. That is the same class as F4: a ban never
   stops the human. **New rule:** no bind may hold `~/.config/chaperone`,
   or the persona could choose the next launch's binds; `_enter` refuses
   it too.
3. **`include`, `exclude` and `access` are not built.** Sharing through
   groups is plain `chgrp`/`chmod`. `chaperone bind` with no arguments
   lists the binds.
4. **Exactly one profile.** HOME carries one group, so a second profile's
   persona could not traverse it. Profiles come back with a traverse group
   per human (projects, above).
5. **Removed:** the profile layer (`home_readonly`, `writable`, `hidden`),
   and the "profiles sharing an account agree on limits" check, now dead
   with one profile.
6. **Walk budget** is a constant (100,000 entries), not a policy key.
7. **The launch directory is not bound automatically.** In HOME the
   persona can start only in a bound directory, or one open to it through
   groups or others. Otherwise the error says to bind it.
8. **Startup files must be readable by others** (`0644`, the default), as
   the persona reads them through the real HOME.
9. **Postulate 4 is rewritten** to match this design (AGENTS.md).

Local e2e (Ubuntu 24.04 container with `/dev/fuse`, `SYS_ADMIN`,
`SYS_PTRACE`): all checks pass. Without `SYS_PTRACE`, root cannot read the
persona's namespace links and session cleanup misses its processes. That
is a property of the container; real root has the capability.

## Phase 1 site pilot (EL9 login node, CephFS HOME, real FreeIPA and Slurm)

All phases pass: identity and groups (own group only, banned group closed),
HOME `0710` (no listing, no writes, `0600` files closed), binds (user `rw`,
`--ro`, refused for a banned group and for the binds file's directory),
writes and atomic `0600` renames landing as the human, locks both ways,
parallel sessions, no sudo, the view unreachable, real Claude Code on the
human's login (also three sessions at once), exit codes and SIGTERM,
cleanup. Slurm on every partition, an array and `srun`: the persona's UID
and groups, cwd HOME, startup files readable, the banned data closed. Jobs
from HOME now work.

Findings:

1. Startup files that are `0600` hide the human's environment. Sharing
   them with the persona group (`0640`) fixes it. Open: a user step
   (README) or done by `_enter` like HOME's group.
2. Jobs cannot use binds: their directories must be shared through groups.
3. `~/.claude.json` stays unreadable to the persona (deferred). Headless use
   is unaffected; the interactive first run is not tested.
4. Codex was not available on the pilot account: not tested.
