# Group-only Chaperone: feasibility study

**Question.** Can Chaperone use nothing but FreeIPA identities, Unix groups,
`chgrp`/`chmod`/setgid and unmodified Slurm, without bindfs, FUSE, mounts,
namespaces, ACLs or Slurm changes, and still meet the 15 hard requirements?

**Verdict: C, impossible under the stated constraints.** Two indispensable
requirements fail on Unix semantics and on how the harness writes its files,
not on missing code. With the five relaxations in [Verdict](#verdict) the
design becomes **B, feasible with compromises**. It is then much smaller than
today's Chaperone, and Slurm works natively.

Evidence: [TEST_MATRIX.md](TEST_MATRIX.md), produced by
[`tests/feasibility/run.sh`](../../tests/feasibility/run.sh). The script runs
in a throwaway container with local users standing in for FreeIPA users, a
real single-node Slurm 24.11 with MUNGE, and optionally the real Claude Code
binary. Latest run: 70 PASS, 30 FAIL, 11 NOT TESTED/INFO. A FAIL is a
finding: it means a requirement is not met.

## Does this already exist?

Yes. The proposal is the classic Unix project-group pattern: one group per
collaboration, setgid directories, umask 002 and `sudo -iu`, here applied to
a second identity per human. Nothing new needs inventing, and that is its
strength. Its weaknesses are just as old:

- a file has exactly one group;
- the mode bits a program chooses win over setgid inheritance;
- supplementary groups are fixed when a process starts.

Same-UID agent sandboxes also exist: Claude Code's sandbox (bubblewrap) and
Codex's sandbox (Landlock and seccomp on Linux). They enforce path
allowlists, and credentials just work because the UID is the human's. They
break requirement 1 (separate UID), bubblewrap breaks requirement 5
(namespaces), and a Slurm job submitted from inside one runs unsandboxed as
the human. They answer a different question and are out of scope here.

## Findings, in order of severity

### F1. Claude Code's login cannot be shared between two UIDs (T05, critical gate)

Claude Code 2.1.306 stores its login with an atomic write in mode `0600` and
then calls `chmod(path, 0o600)` (from the binary's `secureStorage.write`:
`await Rn(n,_(e),384),await Z(n,384)`, where 384 is `0o600`). Every token
refresh therefore produces a new file owned by the refreshing UID with no
group bits. Reproduced:

- T05.1: the human's login (0600) is unreadable to the persona;
- T05.2: after an include (0660) the persona reads it;
- T05.3–5: after one persona refresh the file is `600 alice-ai`, the human
  gets `Permission denied`, and the human cannot `chmod` it back (not the
  owner).

No setgid, umask or group trick can survive an explicit `chmod 0600`.
Keeping it working would need repair after every refresh, which is
forbidden, or a mechanism that is not a plain Unix permission (ACLs, an
idmapped or FUSE view, same UID). **The bidirectional shared-login
requirement FAILS.** This is a property of the harness plus Unix
discretionary access control, not an implementation defect.

The real binary also rewrites `.claude.json` by temp file and `rename`, but
it keeps the existing mode (T05.11). That file survives group sharing,
though its owner flips to the persona (T05.13). The persona can then
`chmod 600` it and lock the human out (T05.14). `.claude.json` sits in HOME
unless `CLAUDE_CONFIG_DIR` is set, and a rename there needs HOME to be
writable, which requirement T05.10 forbids.

Codex 0.163.0 behaves differently. `FileAuthStorage::save` opens `auth.json`
with `create|truncate|write` and `mode(0o600)`, which applies only on
create. A refresh rewrites the file in place, so owner and mode survive.
Shared `0660`, it works in both directions (T05.7–8). A fresh logout and
login recreates it `0600` (T05.9), and the include must then be repeated.

**Smallest relaxation:** Claude uses a long-lived token
(`claude setup-token` → `CLAUDE_CODE_OAUTH_TOKEN`). It lives in a file only
the human writes, is shared read-only through the persona group (T05.10),
and is exported by the human's `.bashrc`. The human does one extra step
once; nothing is ever rewritten. Codex: share `auth.json` once, again after
a re-login. Using a real setup-token through the persona is UNVERIFIED here
(no disposable subscription).

### F2. One persona cannot be confined to the current project (T09, critical gate)

A persona in projA and projB reads projB while working in projA (T09.1);
the working directory is not a boundary. Narrowing the supplementary groups
at launch (`setgroups`, T09.2) holds only until the persona runs
`sg projB`. A member needs no password, so the narrowed shell regains
projB (T09.3). `sbatch` from the narrowed shell gets the full NSS group
list back on the compute node (T09.4: `groups=…projA,projB…`). This is
Unix and Slurm by design; only a separate UID per boundary, or MAC, fixes
it. **FAILS.**

**Smallest relaxation:** a security boundary is a persona. Use one persona
per (human, project) where projects must stay apart, which current
Chaperone already supports as profiles (`<user>-<profile>`, `-p`). Otherwise
accept that a persona has the union of its approved groups.

### F3. Non-recursive include cannot open existing contents (T03)

`chgrp` plus `chmod 2770` on the named directory opens only that directory.
Existing children keep their group and mode (T03.2–4). Setgid passes on the
group but not the mode:

- with the human's umask 022, new human files are read-only to the persona
  and new subdirectories are closed to it (T03.8–9);
- umask 002 fixes that (T03.10);
- a tool that chooses `0600` still closes the file (T03.11);
- a file moved in keeps its old group (T03.12).

**Without recursion only these are guaranteed:** the root opens, and new
entries get the share group. Persona-created entries are fully usable by the
human (persona umask 007, T03.6–7). `include -R` (T04) handles existing
data: it refuses on foreign owners, protected groups, mounts and over-budget
trees before changing anything, it does not follow symlinks, and it keeps a
journal that restores every mode and group exactly (T04.1–10). The prototype
walks the tree 5 times (count, preflight, journal, change, setgid) and makes
2 changes per entry. A real implementation needs one read walk plus 2 writes,
about 3 metadata operations per entry, so 1 M entries means about 3 M MDS
operations. Hence preview, budget and explicit confirmation.

### F4. A banned group bans membership, not the data (T06)

Membership bans hold: no `sg` into a banned group (T06.1), no `chgrp` to it
(T06.2), banned data stays closed (T06.3). But the human owns their files,
and Unix lets an owner `chgrp` to any of their groups. Re-grouping
(T06.4) or copying (T06.5) banned-group data into a share opens it to the
persona. `include` can refuse such paths, but that is cooperative: plain
`chgrp`/`cp` bypasses it. The stronger meaning needs MAC (SELinux MCS or
similar). A CephFS mount carries one `context=` label, so MAC is not
available there. Every DAC design has this hole, including bindfs and ACLs.
Nested FreeIPA groups re-adding a ban (T06.6) were not testable with local
groups; the broker must check indirect membership (`memberof` includes
indirect groups).

### F5. One file has one group (T07, T08)

Sharing part of an existing project with someone outside the project means a
`chgrp`, which overwrites the project group (T07.7: a project member loses
access). Reusing the project group (adding the persona to projA) works
cleanly: GID unchanged, collaborators keep access, and removing one persona
leaves the others working (T07.1–5). For a folder used by two projects, a
dedicated resource group locks out the original project's humans unless they
are in it (T08.3). With **FreeIPA nested groups**, make projA and projB
members of the resource group, and both projects' humans and personas
inherit it with no sync. That is the native answer, NOT TESTED here (local
groups do not nest). Distinct personas with disjoint groups keep A and B
apart while sharing the resource (T08.2, T08.4).

### F6. "Only explicitly included paths" is not enforceable (T10)

Groups can only add access. The persona also gets everything open to
"other":

- other users' world-readable files (T10.2), including through symlinks
  from a share (T10.3);
- every `0644` file under its own human's HOME whose name it knows, because
  HOME must be `0710` for traversal (T10.5). Under umask 022 that is most of
  HOME.

Files `0700`/`0600` stay closed (T10.1, T10.4). **Achievable guarantee: no
additional private access.** A strict allowlist is not.

### F7. Revocation is not immediate (T12)

- A removed group stays in running sessions (T12.2) and running jobs
  (T12.1); supplementary groups are fixed at `exec`.
- New sessions lose it at once (T12.3).
- New Slurm jobs keep it for up to `GroupUpdateTime`: Slurm's group cache
  (`group_cache.c`, `expiration = time(NULL) + slurm_conf.group_time`)
  served the removed group to new jobs for about 10 minutes (T12.7).
  Shortening that is a Slurm configuration change, so it is not allowed.
- On FreeIPA sites the SSSD cache adds its own delay (`sss_cache -E`;
  T12.5 NOT TESTED).

Immediate revocation needs `scancel` and killing sessions.

### F8. Shared writable agent state is a trust hole (T15)

The persona cannot `chmod` or `chgrp` human files, edit `.bashrc`, create
files in HOME, or change its own groups (T15.1–4, 9). But in a
group-writable `~/.claude` it can replace the human's read-only
`settings.json` (hooks) by rename (T15.5), and the human later runs those
hooks.

The sticky bit stops that (T15.6), but it also stops Claude's own
rename-update of a human-owned `.claude.json` (T15.7). Permissions cannot both allow Claude's
atomic writes and stop file replacement. This is the hole the README
already calls the trust boundary. The fix is a policy (read-only
`settings.json`, hooks off for personas), not architecture.

### What works without compromise

- **Separate UID, same HOME path, only approved groups** (T01.1–3).
- **HOME `0710` with a traverse group** is the smallest secure HOME change
  (T01.5): traverse yes, list no, write no.
- **Same shell environment:** `sudo -iu persona` runs the human's
  `.bash_profile`/`.bashrc` as the persona, never as root (T02.1–7), and
  PATH, user-installed programs, argument boundaries and exit codes
  are preserved (T02.8–9). Nested startup files that are `0600` stay
  private, so they must be shared for the environment to match (T02.3).
- **Native Slurm** (T11): `sbatch`, `srun`, `salloc` and arrays all run as
  the persona with its NSS groups, a workdir in HOME, the human's `.bashrc`
  in login shells, and job files in shares editable by the human. This
  fixes the open defect of the bindfs build (PR #13): batch jobs from HOME
  fail there because compute nodes have no bindfs view.
- **No ACLs, no mounts, no walks during ordinary include:** non-recursive
  include of a 1000-entry directory reads no directory entries (T14.1–3).

## Stop/go matrix

| Requirement | Result | Evidence | Limitation |
|---|---|---|---|
| Shared original HOME | PASS | T01.2, T01.5, T11.3 | HOME's group changes to the persona group, mode 0710 |
| Separate persona UID | PASS | T01.1 | |
| Restricted FreeIPA groups | PASS (local), UNVERIFIED (IPA nesting) | T01.3, T06.1–2 | broker must check indirect membership |
| Same shell environment | PASS | T02.1–9 | 0600 nested startup files must be shared; Lmod NOT TESTED |
| Existing AI login usable | **FAIL** (Claude), PASS (Codex) | T05.1–2, T05.7 | Claude: reads only after include; next refresh breaks it |
| Credential updates both ways | **FAIL** (Claude), PASS (Codex, until re-login) | T05.3–5, T05.7–9 | `chmod 0600` after every Claude write |
| No mounts/FUSE | PASS | T14.3 | |
| No ACLs | PASS | T14.2 | |
| No implicit tree walks | PASS | T14.1 | existing contents stay closed (F3) |
| Explicit recursive sharing | PASS (prototype) | T04.1–10 | O(entries) MDS cost; budget plus preview |
| Native Slurm execution | PASS | T11.1–9 | one node type tested; SSSD on compute nodes NOT TESTED |
| Protected-group restrictions | PASS (membership) / **FAIL** (data) | T06.1–5 | owner can re-group or copy; needs MAC |
| Multiple users per shared resource | PASS | T07.1–6 | via existing project group |
| Resource shared across projects | PASS with nested groups (UNVERIFIED), **FAIL** with flat groups | T08.1–4 | one group per file |
| Project isolation (one persona) | **FAIL** | T09.1–4 | needs one persona per boundary |
| No access to unrelated user data | PASS (private) / **FAIL** (strict allowlist) | T10.1–5 | world/other-readable stays readable |
| Acceptable FreeIPA growth | PASS | [scalability](#t13-freeipa-scalability) | per-project personas multiply |

## Verdict

**C, impossible as stated.** Minimal conflicting sets:

1. {separate UID, the human's existing Claude login shared both ways, no
   ACL/mount/repair}: Claude `chmod 0600`s every write (F1).
2. {one persona per human, per-project isolation, same rights in Slurm, no
   Slurm changes}: groups are cumulative per UID, and `sg` and Slurm
   restore them (F2).
3. {non-recursive include, access to existing subtrees, no ACL}: mode bits of
   existing entries (F3).

Requirements 12 and 13, and the strict reading of "AI only sees shared
folders", also fail (F4–F6). They fail for every DAC design, including
today's.

**B, feasible with compromises**, if the maintainer accepts:

| # | Relaxation | Security impact |
|---|---|---|
| R1 | Claude uses a `setup-token` long-lived token, shared read-only; Codex shares `auth.json` once (again after a re-login) | the token is readable by the persona, as the login is today; one manual step per human |
| R2 | one persona per security boundary (per project only where needed) | none; more identities |
| R3 | `include` opens the root and new entries; existing contents need `include -R`; collaboration needs human umask 002 | `include -R` costs O(entries) |
| R4 | `ban_groups` bans membership only | **significant**: a human can re-group or copy banned data to a share |
| R5 | guarantee is "no additional private access", not an allowlist | **significant**: persona reads other-readable files, including unshared 0644 files in its human's HOME |
| R6 | revocation applies to new sessions; new jobs within `GroupUpdateTime`; running work needs kill/`scancel` | delayed revocation |

R4 and R5 are not harmless; they are the price of having no MAC or ACL.
Current Chaperone pays R4 too. It avoids R5 only because bindfs or ACLs
open HOME selectively.

## T13 FreeIPA scalability

100 researchers, 5 projects each, 20 shared resources (each used by about 5
projects, 25 personas), 3 profiles. The persona group doubles as HOME
traverse group and share group. Project groups are reused.

| Model | Users | Groups | Memberships | Slurm assoc. | Isolation |
|---|---|---|---|---|---|
| 1 persona per researcher | 100 | 100 | ~1,100 | 100 | none between projects |
| per researcher × profile | 300 | 300 | ~3,300 | 300 | by profile |
| per researcher × project | 500 | 500 | ~1,500 | 500 | per project |
| per researcher × project × profile | 1,500 | 1,500 | ~4,500 | 1,500 | full |
| + per-resource groups, flat | +0 | +20 | +~1,000 synced by hand | | drifts |
| + per-resource groups, nested | +0 | +20 | +~100 (project groups as members) | | no sync |

FreeIPA handles all of these comfortably. The cost is lifecycle: every
persona needs provisioning, retirement and a Slurm association. Choose model
1, add per-project personas only for projects that must not meet, and use
nested resource groups.

## Answers

1. **Can separate UIDs share Claude/Codex credentials without ACLs, mounts
   or repair?** Claude: no (F1). Codex: yes, until a re-login.
2. **Non-recursive sharing of existing subtrees?** No. Only the root and new
   entries (F3).
3. **Can excluded groups stop access when a human shares protected data?**
   No. Only membership is banned (F4).
4. **One persona, several projects, isolated to the current one?** No (F2).
5. **Resources shared among projects without breaking existing group
   ownership?** With FreeIPA nested groups, yes, but the folder's group
   changes once. With flat groups, no (F5).
6. **Immediate revocation for running sessions and jobs?** No (F7).
7. **"Only included paths" or just no additional private access?** Only the
   latter (F6).
8. **Identities and groups needed?** One persona plus one group per human;
   extra personas only per real boundary; reuse project groups; nested
   resource groups where needed.
9. **Less complexity than current Chaperone?** Yes, a lot. The ACL walker,
   bindfs and mount namespace, compute-node requirement and grant repair all
   go (see [SIMPLIFICATION_PLAN.md](SIMPLIFICATION_PLAN.md)). The price is
   R1–R6.
10. **Requirements to relax:** R1–R6 above. R1 and R2 are the critical
    gates.
