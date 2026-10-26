# Design: the persona gets a HOME of its own

Status: built (PR persona-home). Decided 2026-10-23; replaces binds (bindfs).

## Why

Agent harnesses keep their state per user: `0600` credentials replaced by
rename on every token refresh, daemons with per-UID sockets, hooks, MCP
servers and locks in the same directories. Sharing that state between the
human and a second UID took bindfs, lock forwarding, a mount namespace per
session and a fix per harness feature, and each harness release can break
it again (example: a Codex daemon started by the human, reachable through
the shared state directory, refuses the persona; opening its socket would
let the persona run commands as the human).

The persona becomes an ordinary second user with its own HOME, inside the
human's. The harnesses then work as they do for any user.

## Decisions

| Question | Decision |
| --- | --- |
| Persona HOME | `~/.chaperone/<user>-<profile>`, in the human's HOME |
| Logins | the persona's own: by its own login, or a copy of the human's when the site's lists name it (2026-10-26) |
| Profiles | several: one persona, HOME and login per profile; one traverse group per human |
| Harness settings | copied on request (`chaperone seed`), from site-defined lists of any tool's paths |
| Human's environment | the persona sources the human's startup files |
| Data | groups and `include`/`exclude`, as now |

**Given up:** the human's login, memories and history shared live. The
persona has its own, seeded from a snapshot of the human's.

## Layout

```
~                          alice:alice-chaperone 0710   (0750 with list_home)
~/.bashrc ...              alice:alice-chaperone 0640   (startup files, r--)
~/.chaperone               root:root 0711               created by _enter; only root changes its entries
~/.chaperone/alice-ai      alice-ai:alice-ai 2770       persona HOME, profile ai
~/.chaperone/alice-proja   alice-proja:alice-proja 2770 persona HOME, profile proja
    .bashrc .bash_profile  generated once: source the human's
    .claude/ .claude.json .codex/ ...   that persona's own harness state
```

- **Traverse group `<user>-chaperone`:** the broker creates it and makes
  every persona of the human a member. HOME and the startup
  files carry it, so every profile traverses HOME and reads the startup
  files, and nobody else does (HOME `0711` would open it to all users).
- **Persona group `<user>-<profile>`:** the persona's primary group, of its
  HOME and of what `include -p PROFILE` shares. Personas do not hold each
  other's groups, so profiles do not see each other's HOME or includes.

- FreeIPA `homeDirectory` of the persona is that path, so jobs on every node
  get it; no mount, no bind.
- The human is in the persona's group and the persona's umask is 007: what
  it writes stays readable and writable by the human. A harness may still
  create a `0700` directory; the human removes it with
  `chaperone -- rm -rf …` (running as the persona), and purge does too.
- Quota and usage accounting count the persona under the human's HOME.

## Commands (the human)

```sh
chaperone [-p PROFILE] [CMD ...]            # launch (default profile from the policy)
chaperone [-p PROFILE] seed [HARNESS ...]   # copy harness settings into that persona's HOME
chaperone [-p PROFILE] include [-R] PATH    # share PATH with that persona's group
chaperone [-p PROFILE] exclude PATH
chaperone [-p PROFILE] include              # list
```

The site installs Chaperone with its playbook (`deploy/install.yml`); a
human installs nothing. The first launch of a profile creates its persona
and HOME.

## Launch

`_enter`, as now (eligibility, provision, groups, `no_new_privs`, its own
terminal), with these changes:

1. **Persona HOME.** If missing, create `~/.chaperone` (root's, `0711`, so
   the human cannot swap what root creates in it) and
   `~/.chaperone/<persona>` through descriptors: open HOME, then
   `mkdirat`/`openat` with `O_NOFOLLOW|O_DIRECTORY`, `fchown` and `fchmod` on
   the new descriptor. A symlink or foreign owner at either level refuses the
   launch. A new HOME is built under a temporary name, with its startup
   files (`.bash_profile`, `.bashrc`, `.profile`), and renamed into place
   complete; a parallel first launch then finds it whole.
2. **Startup files.** The persona's `.bashrc` sources the human's with the
   human's HOME, then sets HOME back:
   ```sh
   HUMAN_HOME=/home/alice
   HOME=$HUMAN_HOME; . "$HUMAN_HOME/.bashrc"; HOME=/home/alice/.chaperone/alice-ai
   ```
   so `PATH=$HOME/.local/bin` and the like resolve to the human's tools.
   The human's startup files stay shared read-only (`startup_files`, as now).
3. **Environment.** `HOME` is the persona's HOME; `env` of the profile as
   now. `checkPersona` requires the persona's NSS HOME to be that path.
4. **No mount namespace, no bindfs.** The session is the command run as the
   persona; leftovers are killed by UID as now.

Binaries are not copied: the persona runs the human's installs through
PATH. A harness's self-updater cannot write the human's install; turn it
off in the profile's `env` (site value).

## `chaperone seed`

```sh
chaperone seed                 # on a terminal: offers each harness found in your HOME
chaperone seed claude codex    # copies those harnesses' lists
```

Policy, per profile (site values; the code has none). A harness is any
tool: a name and the HOME-relative paths to copy.

```json
"harnesses": {
  "claude":   [".claude", ".claude.json"],
  "codex":    [".codex"],
  "opencode": [".config/opencode", ".local/share/opencode"],
  "nvim":     [".config/nvim", ".local/share/nvim"]
}
```

- Runs as two unprivileged halves: `tar -C ~ -c LIST` as the human, piped
  into `tar -C $HOME -x` as the persona (through `_enter`, like any
  command). Neither side can touch what its user may not; symlinks are
  stored, not followed. No root file operation in either HOME.
- Paths missing in the human's HOME are skipped; existing persona files are
  overwritten (a re-seed is a fresh snapshot).
- First launch on a terminal with an unseeded HOME asks once which
  harnesses to seed.
- A list that names a login (`.claude/.credentials.json`, `.codex/auth.json`)
  copies it: the persona starts logged in as the human's account. Tokens
  rotate: when one side refreshes, the other's copy may stop working; then
  seed again or log in as the persona. Lists that leave logins out mean the
  persona logs in itself (`chaperone claude` then `/login`;
  `chaperone codex login --device-auth`).
- A launch from the human's HOME starts in the persona's: tools read
  `./.<tool>/` as project config, and the human's is not readable.

Hooks and MCP servers come along in `settings.json`; they run as the
persona. The persona cannot write the human's harness state, so the
earlier escape (a persona hook the human later runs as themself) is gone.

## Removed

bindfs and its package, the mount namespace, `bind`/`unbind`, `bind`,
`bind_ro`, `user_bind`, the ban walk of binds, `~/.config/chaperone/binds`.
Most of `session.go`.

## Kept

Identity and groups (`ban_groups`/`groups`, synced every launch), the sudo
switch, HOME traverse and startup-file sharing with its record, `list_home`,
`include`/`exclude`, Slurm accounts and limits, retire/purge.

## Uninstall

- **Retire** (default): persona disabled; its HOME stays, owned by it.
- **Purge:** `_enter` runs `rm -rf` of the persona HOME *as the persona*
  (it cannot reach anything else), then root removes the empty directory
  through a descriptor, then the account. `~/.chaperone` goes when empty.
- HOME and startup files get their group and mode back, as now.

## Migration from the bind build

- The broker sets `homeDirectory` to the new path (`user_mod`); its FreeIPA
  right must cover that attribute. Existing personas move at their next
  launch.
- `~/.config/chaperone/binds` is ignored; uninstall leaves it.
- Previously seen harness state stays where it was: in the human's
  directories, owned by the human.

## Postulates to change

- **4** becomes: the persona has its own HOME inside the human's, which it
  traverses; it reads the startup files and what others may; data reaches
  it through groups and `include`. No binds, no mounts, no ACLs.
- **5** becomes: the persona has the human's environment (startup files)
  and a seeded copy of the human's harness state, logins as the lists say.

## Tests

CI (e2e, disposable FreeIPA and Slurm):

- persona HOME created with the modes above; a symlink planted at
  `~/.chaperone` or at the persona HOME refuses the launch and changes
  nothing outside;
- the persona's login shell has the human's PATH, `HOME` is the persona's;
- `seed` copies the listed paths as the persona's, skips missing ones,
  stores symlinks without following them; credentials outside the lists stay
  uncopied;
- the persona cannot write the human's harness directories;
- a fake harness daemon started by the human and one by the persona do not
  see each other's sockets;
- a job's HOME is the persona's on a compute node;
- retire keeps the persona HOME; purge removes it as the persona and leaves
  nothing it owned in the human's HOME.

Site pilot (test account first): real `claude /login` and
`codex login --device-auth` as the persona, the Codex daemon, a prompt that
writes in an included directory, a job from it, a hook in the human's
settings not writable by the persona.

## Open

- History files the human's `.bashrc` points at (`HISTFILE=$HOME/...` is
  evaluated with the human's HOME, then unwritable): set it after sourcing.
- Harness self-updaters: which `env` each needs off (site docs).
- Phase-1 deployments carry HOME's group `<user>-ai`; the move to
  `<user>-chaperone` re-groups HOME and startup files once (record kept).
- `include` under HOME needs the traverse group on each directory above
  PATH; `include` of `~/proj/a` gives `~/proj` traverse? Proposed: refuse
  and say which parent to open, rather than change parents silently.
