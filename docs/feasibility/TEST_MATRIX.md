# Group-only Chaperone: test matrix

Generated from one run of [`tests/feasibility/run.sh`](../../tests/feasibility/run.sh)
(Debian trixie container, Slurm 24.11.5 with MUNGE, Claude Code 2.1.306
mounted at `/opt/claude`). Analysis: [FEASIBILITY.md](FEASIBILITY.md).

## Reproduce

```sh
docker run --rm --hostname feas1 --tmpfs /srv/t04m/tree/mnt \
  -v "$PWD/tests/feasibility:/t:ro" \
  -v "$(readlink -f "$(command -v claude)"):/opt/claude:ro" \
  debian:trixie bash /t/run.sh
```

The container needs network for `apt` and takes about 12 minutes; T12.7
waits for Slurm's group cache to expire. Without the Claude mount, T05.11–14
are NOT TESTED. The script refuses to run outside a container.

## Setup

**Users and groups:**

| Who | Groups |
|---|---|
| humans `alice` | projA, projB, clinical |
| `bob` | projA |
| `carol` | unrelated user |
| personas `alice-ai` | HOME `/home/alice`; alice-home, alice-share, projA |
| `bob-ai` | HOME `/home/bob`; bob-share, projA |
| `alice-ai-b` | HOME `/home/alice`; alice-home, projB |

`admins`, `editors` and `clinical` are the protected groups. All HOMEs
start at mode `0700`.

**How each side runs:**

- A human runs with umask 022, a persona with umask 007. Both go through
  `sudo -H -u`, so groups come from NSS via initgroups.
- Startup-file tests use `sudo -iu`.

**Reading the results:** each row is one requirement. FAIL means the
requirement is not met: a finding, not a script error. `blocked:` rows
require the action to fail. INFO rows record state.

## Results

| ID | Requirement checked | Result | Observed | Cause of a FAIL |
|---|---|---|---|---|
| T01.1 | different UIDs (1000 / 1003) | PASS |  |  |
| T01.2 | identical HOME path | PASS |  |  |
| T01.3 | only approved groups: alice-ai projA alice-home alice-share | PASS |  |  |
| T01.mode0700 | HOME 0700 alice:alice-home | INFO | traverse=no list=no write=no |  |
| T01.mode0710 | HOME 0710 alice:alice-home | INFO | traverse=yes list=no write=no |  |
| T01.mode0750 | HOME 0750 alice:alice-home | INFO | traverse=yes list=yes write=no |  |
| T01.mode0770 | HOME 0770 alice:alice-home | INFO | traverse=yes list=yes write=yes |  |
| T01.5 | smallest secure HOME mode: 0710 with a dedicated traverse group (traverse, no list, no write) | PASS |  |  |
| T01.4 | human HOME group change is the only HOME edit (mode 0710, group alice-home) | PASS |  |  |
| T02.1 | login shell (sudo -iu) reads the human's .bash_profile and .bashrc | PASS |  |  |
| T02.2 | PATH from the human's .bashrc | PASS |  |  |
| T02.3 | nested 0600 file stays private (persona env lacks PRIVATE_VAR: such files must be shared to match the human) | PASS |  |  |
| T02.4 | .bashrc ran as the persona, not as root or the human | PASS |  |  |
| T02.5 | no startup file ran as root | PASS |  |  |
| T02.6 | interactive non-login shell reads .bashrc | PASS |  |  |
| T02.7 | non-interactive shell reads no startup file (same as for the human) | PASS |  |  |
| T02.8 | user-installed program via persona PATH: argv boundaries and exit code kept | PASS |  |  |
| T02.9 | python --version as the persona | PASS |  |  |
| T02.10 | environment modules (Lmod) initialisation | NOT_TESTED | no Lmod in this fixture; /etc/profile.d runs for login shells as above | fixture: no Lmod |
| T03.1 | persona lists and enters the shared root | PASS |  |  |
| T03.2 | persona reads existing descendant 0640 (group alice) | FAIL | cat: /home/alice/proj/f0640: Permission denied | Unix: mode bits of existing entries |
| T03.3 | persona writes existing descendant 0660 (group alice) | FAIL | sh: 1: cannot create /home/alice/proj/f0660: Permission denied | Unix: mode bits of existing entries |
| T03.4 | persona enters existing subdirectory 0750 (group alice) | FAIL | ls: cannot open directory '/home/alice/proj/d0750': Permission denied | Unix: mode bits of existing entries |
| T03.5 | persona reads existing 0640 file of another group it holds (projA) | PASS |  |  |
| T03.6 | new persona file inherits the share group (setgid) | PASS |  |  |
| T03.7 | human edits persona-created file (persona umask 007) | PASS |  |  |
| T03.8 | persona edits a file the human creates later (human umask 022) | FAIL | sh: 1: cannot create /home/alice/proj/by-human: Permission denied | Unix: setgid passes the group, not the mode (umask 022) |
| T03.9 | persona creates files in a subdirectory the human creates later (umask 022) | FAIL | touch: cannot touch '/home/alice/proj/dir-by-human/x': Permission denied | Unix: setgid passes the group, not the mode (umask 022) |
| T03.10 | ...same with human umask 002 | PASS |  |  |
| T03.11 | persona reads a new file the human's tool creates with explicit mode 0600 | FAIL | cat: /home/alice/proj/explicit0600: Permission denied | Unix: explicit mode chosen by the program |
| T03.12 | persona writes a file the human moves in (rename keeps the old group) | FAIL | sh: 1: cannot create /home/alice/proj/moved-in: Permission denied | Unix: rename keeps the inode group |
| T04.1 | refuses a tree that crosses a filesystem boundary | PASS |  |  |
| T04.2 | operation budget: preview count refuses a large tree, nothing changed | PASS | refuse: 21 entries > budget 5 |  |
| T04.3 | protected group inside the tree: refused before any change | PASS |  |  |
| T04.4 | include -R exits 0 on full success | PASS |  |  |
| T04.5 | persona reads existing 0600 descendant after include -R | PASS |  |  |
| T04.6 | persona enters existing 0700 subdirectory after include -R | PASS |  |  |
| T04.7 | symlink target outside the tree untouched | PASS |  |  |
| T04.8 | metadata syscalls: 497 for 21 entries (~23 per entry, incl. sudo/sh overhead) | PASS |  |  |
| T04.9 | journal restores every mode and group exactly | PASS |  |  |
| T04.10 | foreign-owned descendant: refused, nonzero exit, nothing changed | PASS |  |  |
| T05.1 | persona reads the human's Claude login (as Claude writes it: 0600) | FAIL | cat: /home/alice/.claude/.credentials.json: Permission denied | harness: Claude writes 0600 |
| T05.2 | persona reads it after include (0660 alice-share) | PASS |  |  |
| T05.3 | after persona token refresh: 600 alice-ai:alice-share | INFO |  |  |
| T05.4 | human still reads the login after the persona refreshes it | FAIL | cat: /home/alice/.claude/.credentials.json: Permission denied | harness: Claude chmod 0600 after atomic write |
| T05.5 | human can repair it without root (owner is now the persona) | FAIL | chmod: changing permissions of '/home/alice/.claude/.credentials.json': Operation not permitted | Unix: only the owner may chmod |
| T05.6 | HOME stays non-writable for the persona | PASS |  |  |
| T05.7 | Codex: human reads auth.json after a persona refresh (owner stays human, 0660) | PASS |  |  |
| T05.8 | Codex: persona reads auth.json after a human refresh | PASS |  |  |
| T05.9 | Codex: persona reads auth.json after the human logs out and in again | FAIL | cat: /home/alice/.codex/auth.json: Permission denied | harness: Codex creates auth.json 0600 |
| T05.10 | relaxation: persona reads a long-lived token file (CLAUDE_CODE_OAUTH_TOKEN) | PASS |  |  |
| T05.11 | real Claude: shared .claude.json (alice, 0660) after a persona run: 660 alice-ai:alice-share | INFO |  |  |
| T05.12 | real Claude: human still writes .claude.json after a persona rewrite | PASS |  |  |
| T05.13 | real Claude: human keeps ownership of .claude.json | FAIL |  | harness: rename makes the writer the owner |
| T05.14 | blocked: real Claude: persona cannot lock the human out of .claude.json | FAIL | succeeded: | Unix: owner may chmod |
| T06.1 | blocked: persona joins a banned group with newgrp/sg | PASS | sg: getline() failed: Inappropriate ioctl for device Password: |  |
| T06.2 | blocked: persona chgrps its own file to a banned group | PASS | chgrp: changing group of '/tmp/ai-file': Operation not permitted |  |
| T06.3 | blocked: persona reads data protected by a banned group | PASS | cat: /home/alice/clin/sample: Permission denied |  |
| T06.4 | blocked: human (owner) cannot re-group banned data to a share group outside Chaperone | FAIL | succeeded: patient | Unix: owner may chgrp to own groups (needs MAC) |
| T06.5 | blocked: banned-group data copied by the human into a share stays closed | FAIL | succeeded: patient | Unix: copying data (needs MAC) |
| T06.6 | FreeIPA nested groups re-adding a banned group | NOT_TESTED | local groups do not nest; see FEASIBILITY.md | fixture: local groups do not nest |
| T07.1 | both personas work in an existing project group | PASS |  |  |
| T07.2 | project GID unchanged | PASS |  |  |
| T07.3 | human collaborator edits a persona file | PASS |  |  |
| T07.4 | removing one persona leaves the other working | PASS |  |  |
| T07.5 | blocked: removed persona loses access (new session) | PASS | cat: /srv/projA/data: Permission denied |  |
| T07.6 | blocked: alice-ai cannot reach bob's own share | PASS | cat: /home/bob/share/x: Permission denied |  |
| T07.7 | sharing one subdir through a new group keeps project members' access | FAIL | cat: /srv/projA/sub/s: Permission denied | Unix: one group per file |
| T08.1 | projB persona reads shared-reference owned by projA | FAIL | cat: /srv/shared-ref/r: Permission denied | Unix: one group per file |
| T08.2 | dedicated resource group: projB persona reads it | PASS |  |  |
| T08.3 | dedicated resource group: projA humans keep access without joining it | FAIL | cat: /srv/shared-ref/r: Permission denied | Unix: one group per file (nested IPA groups would fix, not tested) |
| T08.4 | blocked: distinct personas: projB persona cannot read projA | PASS | cat: /srv/projA/data: Permission denied |  |
| T09.1 | blocked: persona started for projA cannot read projB | FAIL | succeeded: secretB | Unix: groups are per UID, cwd is no boundary |
| T09.2 | blocked: launcher narrows groups to projA (setgroups): projB closed | PASS | cat: /srv/projB/secret: Permission denied |  |
| T09.3 | blocked: narrowed persona cannot regain projB with sg (member, no password) | FAIL | succeeded: secretB | Unix: members may sg without password |
| T09.4 | batch job from a projA-narrowed launch cannot read projB | FAIL | uid=1003(alice-ai) gid=1012(alice-ai) groups=1012(alice-ai),1000(projA),1001(projB),1006(alice-home),1007(alice-share) secretB | Slurm: job groups come from NSS |
| T11.1 | sbatch runs as the persona UID | PASS |  |  |
| T11.2 | job supplementary groups = NSS groups of the persona | PASS |  |  |
| T11.3 | job with workdir = shared HOME (0710) reads startup files; stdout in a shared dir | PASS |  |  |
| T11.4 | blocked: job cannot create files in HOME itself | PASS |  |  |
| T11.5 | job-created file in a shared dir: share group, human can edit | PASS |  |  |
| T11.6 | srun as the persona | PASS |  |  |
| T11.7 | salloc + srun as the persona | PASS |  |  |
| T11.8 | job array as the persona | PASS |  |  |
| T11.9 | login shell in a job reads the human's .bashrc | PASS |  |  |
| T11.10 | several node types, SSSD on compute nodes | NOT_TESTED | single container node | fixture: one container node |
| T12.7 | new job after a group removal has no access | FAIL | Slurm reused its cached group list; still cached after 580 s more (gone=yes) | Slurm: group cache, GroupUpdateTime (600 s default) |
| T12.1 | running job loses a group removed while it runs | FAIL | job still read projB after removal | kernel: groups fixed at exec |
| T12.2 | running interactive session loses a removed group | FAIL | Groups:	1000 1001 1006 1007 1012 | kernel: groups fixed at exec |
| T12.3 | blocked: new session after removal has no access | PASS | cat: /srv/projB/secret: Permission denied |  |
| T12.4 | group added before launch is effective at once (no cache here) | PASS |  |  |
| T12.5 | SSSD cache delay | NOT_TESTED | no SSSD; FreeIPA sites need sss_cache -E or wait entry_cache_timeout | fixture: no SSSD |
| T12.6 | human can chmod a file the persona created (e.g. to clean up after retirement) | FAIL | chmod: changing permissions of '/home/alice/proj/by-ai': Operation not permitted | Unix: only the owner may chmod |
| T10.1 | blocked: persona reads a 0700 HOME of another user | PASS | ls: cannot open directory '/home/carol': Permission denied |  |
| T10.2 | blocked: persona reads another user's world-readable file (strict allowlist) | FAIL | succeeded: pub | Unix: other bits; groups only add access |
| T10.3 | blocked: persona follows a symlink from a share to another user's readable file | FAIL | succeeded: pub | Unix: other bits; groups only add access |
| T10.4 | blocked: symlink to another user's private file | PASS | cat: /home/alice/proj/link-priv: Permission denied |  |
| T10.5 | blocked: persona reads an unshared 0644 file of its human (HOME 0710, name known) | FAIL | succeeded: mine | Unix: HOME 0710 exposes other-readable files by name |
| T14.1 | non-recursive include of a 1000-entry dir reads no directory entries | PASS |  |  |
| T14.2 | no ACL anywhere after all tests | PASS |  |  |
| T14.3 | no mounts under /home or /srv | PASS |  |  |
| T15.1 | blocked: persona chmods a human file | PASS | chmod: changing permissions of '/home/alice/proj/f0644': Operation not permitted |  |
| T15.2 | blocked: persona chgrps a human file | PASS | chgrp: changing group of '/home/alice/proj/f0644': Operation not permitted |  |
| T15.3 | blocked: persona edits the human's .bashrc | PASS | sh: 1: cannot create /home/alice/.bashrc: Permission denied |  |
| T15.4 | blocked: persona creates a file in HOME (e.g. .bash_login) | PASS | touch: cannot touch '/home/alice/.bash_login': Permission denied |  |
| T15.5 | blocked: persona replaces the human's read-only settings.json (hooks) in a shared dir | FAIL | succeeded: | Unix: write on a directory allows replacing entries |
| T15.6 | blocked: sticky shared dir: persona replaces settings2.json | PASS | mv: cannot move '/home/alice/.claude/s2.tmp' to '/home/alice/.claude/settings2.json': Operation not permitted |  |
| T15.7 | sticky shared dir: persona can still rename-update .claude.json as Claude does | FAIL | mv: cannot move '/home/alice/.claude/cj.tmp' to '/home/alice/.claude/.claude.json': Operation not permitted | Unix: sticky bit also blocks the harness rename |
| T15.8 | blocked: a collaborator re-groups a persona file in a shared project | PASS | chgrp: changing group of '/srv/projA/by-alice-ai': Operation not permitted |  |
| T15.9 | blocked: persona edits its own group memberships | PASS | gpasswd: Permission denied. |  |
| T15.10 | job submitted to another Slurm account | NOT_TESTED | no accounting here; associations are covered by tests/integration | fixture: no Slurm accounting |
