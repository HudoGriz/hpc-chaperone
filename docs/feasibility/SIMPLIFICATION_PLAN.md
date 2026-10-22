# Chaperone: hybrid design (groups everywhere, `bind` on login nodes)

Status: **design, not implemented, not yet prototyped.** It follows from
[FEASIBILITY.md](FEASIBILITY.md):

- groups alone cannot share harness state between two UIDs (F1);
- a mount mapping can share it, for every harness alike;
- only the login node needs that mapping, because Slurm jobs only need data,
  and groups already give jobs the same data rights as the login node (T11).

It keeps relaxations R2–R6 of the verdict and drops R1 (no `setup-token`
needed). Plan requirement 5 ("no mounts") is dropped **for login nodes
only**. Compute nodes and Slurm stay untouched.

## Model

| | Login node (interactive agent) | Compute node (Slurm job) |
|---|---|---|
| Identity | persona UID | persona UID |
| Project data | FreeIPA groups | the same groups, the same rights |
| HOME | `0710`, group = persona group | the same: jobs from HOME work |
| Harness state | `bind`: bindfs mapping, writes land as the human | not visible |
| Mounts | private mount namespace per session | none |

- **Identity:** `<user>-<profile>`, created automatically on first launch.
  Its groups are the profile's approved groups, never banned ones (checked
  including indirect FreeIPA membership), reconciled by the broker on each
  launch.
- **One group per persona:** the human is a member. It is the persona's
  primary group and HOME's traverse group.

## CLI

```sh
chaperone [-p PROFILE] [CMD ARGS…]    # launch; PATH lookup after the identity switch
chaperone bind [--ro] PATH            # map PATH (in HOME) into the persona's sessions
chaperone bind [--ro] SRC:PATH        # map SRC over PATH (per-profile state, below)
chaperone unbind PATH
chaperone include [-R] PATH           # group sharing; works on every node
chaperone exclude PATH
chaperone access                      # binds, includes and groups of a profile
```

All of these run as the human, unprivileged. `bind` and `include` record
the request; `_enter` applies binds at the next launch. Changes apply on the
next launch.

## `bind`: how it works

On the login node, `_enter` (root) runs these steps for each session:

1. Unshare a mount namespace and make `/` a recursive slave, so nothing
   propagates back to the host.
2. Bind the real HOME to a private source path, then mount
   `bindfs --map=human/persona:@human/@persona --enable-lock-forwarding
   --multithreaded -o allow_other,default_permissions` from it onto a
   hidden view. Mounting bindfs over its own source deadlocks, as the lab
   pilot showed.
3. For each bound path, bind-mount `view/PATH` over `HOME/PATH`; `--ro`
   remounts it read-only. **Bind directories, not files.** `rename()` onto a
   mount point fails with `EBUSY`, and harnesses replace their state files
   by rename. Inside a bound directory, renames work.
4. Detach the view and the source (`umount2(MNT_DETACH)`). Only the bound
   paths stay reachable.
5. Drop to the persona (no_new_privs) and run the command. When the
   namespace ends the mounts go, and `_enter` stops its bindfs.

Each session gets its own namespace and bindfs, so parallel sessions are
independent. Locks are forwarded, so human, persona and a second session
lock the same files correctly; the lab pilot showed this with flock, fcntl
and SQLite.

**Defaults.** The profile lists default binds (for example `.claude`,
`.claude.json`, `.codex`, `.config/opencode`). The human adds more with
`chaperone bind` unless the profile sets `"user_bind": false`. There is no
built-in deny list: what may be bound is the profile author's decision, as
with every other profile setting.

**Harness-agnostic.** A bound path behaves for the persona exactly as it
does for the human: same owner, same 0600 files, atomic renames and locks.
No harness is named in code.

**Trust boundary.** A read-write bound path carries the human's authority
over it. Hooks in `settings.json` are the obvious example. Bind those
`--ro` or don't bind them.

## Separate projects without overlap

The kernel separates only by UID. One persona's groups always add up:
`sg` and Slurm restore them (F2), and Slurm jobs ignore the login node's
mounts. So:

**Hard separation: one persona per project boundary.** No admin work per
project:

- a **project profile template** in the policy turns `chaperone -p @projA`
  into the persona `alice-projA`, with exactly one approved group: `projA`;
- it is allowed if the human is in `projA` and `projA` is not banned;
- it is created on first use and then reused, not created per launch;
- its Slurm association follows the template;
- a launch with `@projA` can never reach `projB`, on the login node or in
  any job.

**Same agent, separate memory.** All of alice's project personas bind the
same login and settings, so they feel like one agent: no re-login, same
skills. What would leak between projects is the harness's own history
(transcripts, memories, caches). The template therefore binds a per-project
directory over it:

```json
"templates": {
  "@": {
    "groups": ["{project}"],
    "bind": [".claude", ".chaperone/{project}/claude-projects:.claude/projects",
             ".codex",  ".chaperone/{project}/codex-sessions:.codex/sessions"]
  }
}
```

The second bind of each pair is nested inside the first, so the login and
settings are shared while history is per project. These paths are
configuration, not code. A site can add any harness by naming its history
directories.

**Soft separation: one persona, other projects hidden.** If the projects
only need to stay out of each other's way, not be secret from each other,
one persona with all approved groups is enough. The session hides the other
projects' paths (`tmpfs` over them) and starts in the selected one. This is
**not** a security boundary: the persona still holds the groups, and any
Slurm job it submits sees everything.

**What separation never covers** (as F4–F6):

- world-readable data;
- `0644` files in the human's HOME that a persona can open by name;
- data the human copies across projects.

## Retain, remove, add

| | Component | Change |
|---|---|---|
| keep | `broker.go` | persona, group, association; **plus** project-group reconcile and the template |
| keep | `enter.go` | identity checks, privilege drop, log; **plus** namespace and `bind` steps 1–5 |
| keep | `policy.go` | profiles, templates, default binds, `include -R` budget |
| keep | `uninstall.go` | retire by default; purge chowns persona files back in journaled roots |
| reuse | PR #13 bindfs code | narrowed from all of HOME to the listed paths |
| remove | `acl.go` + tests (~680 lines), grant repair, `setfacl` | no ACLs anywhere |
| remove | bindfs over all of HOME, compute-node bindfs plan | |
| add | `bind`, `unbind`, `include`, `exclude`, `access` | journaled records |

**Least-privilege change.** The broker now edits members of *project* groups.
Its FreeIPA permission must exclude banned and administrative groups by
filter, as PR #13's `permission_mod --ban` does.

## Prototype tests (local container, `--device /dev/fuse --cap-add SYS_ADMIN`)

1. The real Claude and Codex binaries as human and persona, in parallel,
   through `bind` (login, rename, locks, owner stays the human).
2. **Leak check:** after step 4 no path in the session reaches the detached
   view or an unbound HOME file through the mapping (`/proc/*/root`,
   `/proc/*/cwd`, open fds, `/proc/self/mountinfo`).
3. Nested directory binds (`.claude` plus a per-project `.claude/projects`)
   survive Claude's atomic renames and locks.
4. A project template: `@projA` cannot read `projB` interactively or in
   `sbatch`, `srun` or `salloc`; per-project history paths do not cross.
5. A Slurm job from HOME, with no mapping on the compute side.
6. Plus the T01–T15 checks from `tests/feasibility/run.sh`.

## Decided

- **No bind of HOME as a whole**, not even read-only. It would give the
  persona:
  - the human's credentials (SSH keys mean logging in as the human,
    outside Chaperone);
  - owner rights over human-owned files of banned groups;
  - a different view on login nodes than in jobs;
  - FUSE cost on every HOME access.

  Only listed directories are bound.

## Open decisions

- **State files directly in HOME** (`~/.claude.json`): they can't be bound
  (rename onto a mount point gives `EBUSY`), and HOME is not writable for
  the temp file. Options:
  - a profile `env` redirect (`CLAUDE_CONFIG_DIR=~/.claude` puts the file
    inside the bound directory, but the persona then starts from its own
    `.claude.json`);
  - accepting that the persona keeps its own copy.

  Prototype test 1 decides.

- **Hard or soft separation as the default for projects.** The template
  makes hard separation cheap.
- **Whether humans may `bind` beyond the profile defaults.**
- **Naming:** `alice-projA` must stay within FreeIPA's 32-character UID
  limit. The template needs a rule for long project names.
