#!/usr/bin/env bash
# End-to-end on a disposable CI runner: the real install.yml and
# uninstall.yml (login-node plays), real UIDs, sudo, the broker as its
# service user. Only the directory and Kerberos are stubs (ipa, sacctmgr,
# kinit, ipa-getkeytab): tests/integration covers real FreeIPA and Slurm.
# Mutates users, groups, sudoers and /etc: GitHub Actions only.
set -Eeuo pipefail
: "${GITHUB_ACTIONS:?disposable CI runner only}"
: "${RUNNER_TEMP:?}"
BIN=$(realpath "${1:?usage: run.sh path/to/chaperone}")
REPO=$(cd "$(dirname "$0")/../.." && pwd)
H=chaperone-e2e
P=$H-ai
TG=$H-chaperone # the human's traverse group
HH=/home/$H
PH=$HH/.chaperone/$P # the persona's HOME
T=$RUNNER_TEMP/chaperone-e2e
mkdir -p "$T"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
as_h() { sudo -H -u "$H" sh -c 'cd && exec "$@"' sh "$@"; }
as_hin() { local d=$1; shift; sudo -H -u "$H" sh -c 'cd "$1" && shift && exec "$@"' sh "$d" "$@"; }
expect_refusal() { # want-substring, then command
  local want=$1; shift
  rm -f /tmp/e2e-ran
  if out=$(as_h "$@" 2>&1); then fail "accepted: $* ($out)"; fi
  [[ $out == *"$want"* ]] || fail "wrong refusal for $*: $out"
  [[ ! -e /tmp/e2e-ran ]] || fail "command ran despite refusal: $*"
}
playbook() { sudo "$(command -v ansible-playbook)" -i "$T/inventory" -e @"$T/site.yml" "$REPO/deploy/$1" "${@:2}"; }


# --- Fixture: what FreeIPA would hold -----------------------------------
for g in chaperone-users restricted lab; do sudo groupadd "$g"; done
sudo useradd -m -U -G chaperone-users,restricted,lab -s /bin/bash "$H"
sudo chmod 0700 "$HH"  # as FreeIPA's mkhomedir makes it
sudo setfacl -R -b "$HH"  # runner /home has default ACLs, inherited by the skel copy; FreeIPA homes have none
home_mode=$(stat -c %a "$HH") # nothing may change it
# Personas: own UID, own group (the human a member), HOME inside the human's.
sudo useradd -M -U -d "$PH" -s /usr/sbin/nologin "$P"
sudo usermod -aG "$P" "$H"
sudo groupadd "$TG"
for g in restricted lab; do
  sudo install -d -o root -g "$g" -m 0750 "/srv/$g"
  echo "$g-data" | sudo tee "/srv/$g/data" >/dev/null
done
sudo install -d -m 0777 /srv/scratch # world-writable, not sticky
as_h sh -c 'umask 077; echo "export CHAP_LOGIN=from-bash-profile" > .bash_profile; chmod 600 .bash_profile; echo secret > private.txt
  mkdir .ssh .claude && echo key > .ssh/id && echo "{}" > .claude/settings.json
  echo "# notes" > .claude/CLAUDE.md; echo tok > .claude/.credentials.json'

# Stubs the broker calls (PATH puts /usr/local/bin first). ipa keeps persona
# memberships in /tmp/e2e-ipa (the broker writes them as its service user)
# and logs who asked.
stub() { sudo tee "/usr/local/bin/$1" >/dev/null; sudo chmod 0755 "/usr/local/bin/$1"; }
sudo install -d -m 0777 /tmp/e2e-ipa
stub ipa <<STUB
#!/bin/sh
id -un >> /tmp/e2e-broker-calls
m=/tmp/e2e-ipa/\$(echo "\$*" | sed -n 's/.*--users=//p')
gid() { getent group "\$1" | cut -d: -f3; }
groups() { [ -f "/tmp/e2e-ipa/\$1" ] && sed 's/.*/memberof: cn=&,cn=groups,dc=e2e/' "/tmp/e2e-ipa/\$1"; true; }
case "\$1:\$2" in
group-show:$P) printf 'description: chaperone:$H\\ngidnumber: %s\\nmember: uid=$H,cn=users,dc=e2e\\n' "\$(gid "\$2")" ;;
group-show:$TG) printf 'description: chaperone:$H\\ngidnumber: %s\\n' "\$(gid "\$2")" ;;
user-show:$P) printf 'homedirectory: $PH\\nuserclass: chaperone:$H\\ngidnumber: %s\\nmemberof: cn=ipausers,cn=groups,dc=e2e\\n' "\$(gid "\$2")"; groups "\$2" ;;
user-show:$H) [ -e /etc/e2e-ineligible ] || printf 'memberof: cn=%s,cn=groups,dc=e2e\\n' chaperone-users restricted lab $P ;;
user-show:*) echo "ipa: ERROR: \$2: user not found"; exit 2 ;;
group-add-member:*) echo "\$2" >> "\$m" ;;
group-remove-member:*) grep -vx "\$2" "\$m" > "\$m.new"; mv "\$m.new" "\$m" ;;
*) echo "unexpected ipa \$*" >&2; exit 1 ;;
esac
STUB
printf '#!/bin/sh\ncase "$*" in *show*) echo agents ;; *) echo "unexpected sacctmgr $*" >&2; exit 1 ;; esac\n' | stub sacctmgr
printf '#!/bin/sh\ncase "$*" in *MEMORY:check*) test -s /etc/chaperone/chaperone.keytab ;; esac\n' | stub kinit
printf '#!/bin/sh\nwhile [ $# -gt 0 ]; do [ "$1" = -k ] && echo stub > "$2"; shift; done\n' | stub ipa-getkeytab
printf '#!/bin/sh\nexit 0\n' | stub kdestroy

# --- Install with the real playbook ---------------------------------------
sudo apt-get install -y -qq python3-apt >/dev/null
(cd "$REPO" && sha256sum chaperone deploy/* > chaperone.sha256)
[[ $BIN == "$REPO/chaperone" ]] || fail "run from a build in the repository root"
printf '[chaperone]\nlocalhost ansible_connection=local chaperone_fqdn=e2e.test\n[chaperone_ipa]\n[chaperone_slurmdbd]\n' > "$T/inventory"
site() { # $1: ban_groups of profile ai; $2: startup_files, if set; $3: more keys
  cat > "$T/site.yml" <<SITE
chaperone_uid: 960
chaperone_policy:
  default_persona: ai
  eligible_groups: [chaperone-users]
  personas:
    ai: {slurm_account: agents, shell: /bin/bash, ban_groups: [$1], harnesses: {claude: [.claude/settings.json, .claude/CLAUDE.md, .claude/missing]}${2:+, startup_files: $2}${3-}}
SITE
}
site restricted
playbook install.yml
playbook install.yml | tee "$T/second.log"
grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' "$T/second.log" || fail "second install changed something"
ok "install.yml installs, and a second run changes nothing"

# --- Identity and groups ---------------------------------------------------
[[ $(as_h chaperone -- id -un) == "$P" ]] || fail "command did not run as $P"
grep -qx chaperone /tmp/e2e-broker-calls || fail "broker did not run as the service user"
grep -qx root /tmp/e2e-broker-calls && fail "broker ran as root"
ok "runs as the profile's persona; the broker runs as its service user, never root"
sorted() { tr ' ' '\n' | sort | xargs; }
[[ $(as_h chaperone -- id -Gn | sorted) == $(echo "$P lab $TG" | sorted) ]] || fail "ai groups: $(as_h chaperone -- id -Gn)"
[[ $(sorted < "/tmp/e2e-ipa/$P") == $(echo "lab $TG" | sorted) ]] || fail "broker did not give $P the human's groups and the traverse group in the directory"
[[ $(as_h chaperone -- cat /srv/lab/data) == lab-data ]] || fail "persona cannot read the human's group data"
as_h chaperone -- cat /srv/restricted/data 2>/dev/null && fail "persona read a banned group's data"
ok "persona holds the human's groups minus the ban list, and the traverse group"
[[ $(as_h env https_proxy=http://proxy.e2e:3128 chaperone -- sh -c 'echo "${https_proxy-unset} $HOME"') == "unset $PH" ]] || fail "persona env is the caller's, or HOME not the persona's"
[[ $(as_h chaperone -- sh -c 'echo $CHAP_LOGIN') == from-bash-profile ]] || fail "login shell did not source the 0600 .bash_profile"
[[ $(sudo stat -c '%a %G' "$HH/.bash_profile") == "640 $TG" ]] || fail "startup file not shared read-only: $(sudo stat -c '%a %G' "$HH/.bash_profile")"
ok "the persona's environment comes from its login shell, which reads the human's startup files, not the caller"

# --- HOMEs: the human's traversed only; the persona's its own ---------------
[[ $(stat -c '%a %G' "$HH") == "710 $TG" ]] || fail "HOME after a launch: $(stat -c '%a %G' "$HH")"
[[ $(sudo stat -c '%a %U:%G' "$HH/.chaperone" "$PH" | xargs) == "711 root:root 2770 $P:$P" ]] ||
  fail "persona HOME: $(sudo stat -c '%a %U:%G' "$HH/.chaperone" "$PH" | xargs)"
for c in "ls $HH" "cat $HH/private.txt" "cat $HH/.ssh/id" "touch $HH/new.txt" "cat $HH/.claude/settings.json" "touch $HH/.claude/x"; do
  as_h chaperone -- $c 2>/dev/null && fail "persona could: $c"
done
ok "the human's HOME is traversable only (0710, traverse group): its files and harness state stay the human's"
[[ $(as_h chaperone -- sh -c 'pwd && touch mine && stat -c %U:%G mine' | xargs) == "$PH $P:$P" ]] || fail "persona does not start and work in its HOME when launched from the human's"
as_h sh -c "echo h >> $PH/mine" || fail "human cannot write in the persona's HOME"
ok "the persona's HOME is its own, inside the human's; the human may write there"
# The human may rename ~/.chaperone (HOME is theirs) and put something else there.
sudo install -d -m 0755 /tmp/e2e-target
as_h sh -c 'mv .chaperone .chap-real && ln -s /tmp/e2e-target .chaperone'
expect_refusal ".chaperone" chaperone -- touch /tmp/e2e-ran
[[ -z $(ls -A /tmp/e2e-target) && $(stat -c %U /tmp/e2e-target) == root ]] || fail "launch wrote through a planted symlink"
as_h sh -c 'rm .chaperone && mkdir .chaperone'
expect_refusal "owned by UID" chaperone -- touch /tmp/e2e-ran
as_h sh -c 'rmdir .chaperone && mv .chap-real .chaperone'
as_h chaperone -- true || fail "launch after restoring ~/.chaperone"
ok "a ~/.chaperone swapped for a symlink or a directory of the human's refuses the launch"
as_h chaperone seed claude
[[ $(as_h chaperone -- sh -c 'cat ~/.claude/settings.json ~/.claude/CLAUDE.md') == $'{}\n# notes' ]] || fail "seed did not copy the harness settings"
[[ $(sudo stat -c %U:%a "$PH/.claude/settings.json") == "$P:600" ]] || fail "seeded file: $(sudo stat -c %U:%a "$PH/.claude/settings.json")"
sudo test -e "$PH/.claude/.credentials.json" && fail "seed copied a path outside the lists"
[[ $(as_h chaperone seed </dev/null) == "claude: .claude/settings.json .claude/CLAUDE.md" ]] || fail "seed list: $(as_h chaperone seed </dev/null)"
out=$(as_h chaperone seed nope 2>&1) && fail "seeded an unknown harness"
[[ $out == *"unknown harness"* ]] || fail "wrong refusal: $out"
ok "seed copies the profile's harness paths into the persona's HOME as its own; nothing outside the lists"
sudo install -d -o "$H" -g "$H" -m 0755 /srv/scratch/proj
as_hin /srv/scratch/proj chaperone -- sh -c 'echo p > /srv/scratch/out.txt'
[[ $(stat -c %a:%G /srv/scratch/out.txt) == "660:$P" ]] || fail "persona file outside HOME: $(stat -c %a:%G /srv/scratch/out.txt)"
as_h sh -c 'echo h >> /srv/scratch/out.txt' || fail "human cannot write a persona file outside HOME"
ok "outside HOME the persona's files stay group-writable for the human"
as_h sh -c 'umask 077; mkdir -p inc/old && echo s > inc/old/f && echo t > inc/top'
as_h chaperone include inc
[[ $(as_h chaperone -- ls inc | xargs) == "old top" ]] || fail "include: directory not shared"
as_h chaperone -- cat inc/top 2>/dev/null && fail "include without -R shared what the directory held"
as_h chaperone include -R inc
[[ $(as_h chaperone -- sh -c 'cat inc/old/f; echo x > inc/new') == s ]] || fail "include -R: existing contents not shared"
[[ $(sudo stat -c '%a %G' "$HH/inc" "$HH/inc/old/f" "$HH/inc/new" | xargs) == "2770 $P 660 $P 660 $P" ]] ||
  fail "include modes: $(sudo stat -c '%a %G' "$HH/inc" "$HH/inc/old/f" "$HH/inc/new" | xargs)"
as_h sh -c 'echo h >> inc/new' || fail "human cannot write the persona's file in an include"
ok "chaperone include [-R]: the persona's group, group rw, setgid; what it writes there stays the human's to edit"
out=$(as_h chaperone include -R inc 2>&1) && fail "include -R over a persona's file"
[[ $out == *"is not yours"* ]] || fail "wrong refusal: $out"
as_h sh -c 'mkdir -p ban/sub && chgrp restricted ban/sub'
out=$(as_h chaperone include -R ban 2>&1) && fail "included a tree with a banned group"
[[ $out == *"banned group restricted"* ]] || fail "wrong refusal: $out"
for d in . .config .chaperone; do as_h chaperone include -R $d 2>/dev/null && fail "included $d"; done
as_h chaperone exclude inc
[[ $(sudo stat -c '%a %G' "$HH/inc" "$HH/inc/old/f" | xargs) == "700 $H 600 $H" ]] || fail "exclude: $(sudo stat -c '%a %G' "$HH/inc" "$HH/inc/old/f" | xargs)"
[[ -z $(as_h chaperone include) ]] || fail "exclude kept records: $(as_h chaperone include)"
as_h chaperone -- cat inc/top 2>/dev/null && fail "persona reads after exclude"
as_h rm -rf inc
ok "include refuses HOME, its config, the personas' HOMEs, banned groups and others' files; exclude restores every group and mode"
# --- Session ends: nothing of it stays -----------------------------------
[[ $(as_h chaperone -- sh -c 'setsid sleep 300 </dev/null >/dev/null 2>&1 & echo started') == started ]] || fail "leftover fixture"
pgrep -u "$P" >/dev/null && fail "leftover persona process survived its session"
ok "after a session: no persona process"
as_h chaperone -- sleep 5 & pid=$!
for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done
as_h chaperone -- true
pgrep -u "$P" -x sleep >/dev/null || fail "ending one session killed a parallel one"
wait "$pid" || fail "parallel session failed"
ok "parallel sessions: ending one leaves the other running"

# --- Isolation -----------------------------------------------------------
out=$(as_h chaperone -- sudo -n true 2>&1) && fail "persona ran sudo"
[[ $out == *"no new privileges"* ]] || fail "sudo refused for another reason: $out"
ok "no_new_privs: no setuid program (sudo included) works for the persona"
# use_pty: the persona gets a terminal of its own, so it cannot push
# keystrokes (TIOCSTI) into the caller's shell. Sudo before 1.9.14 (EL9)
# does not do this by default.
sudo tee /tmp/e2e-tty.sh >/dev/null <<'TTY'
#!/bin/bash
cd
echo "caller=$(tty) persona=$(chaperone -- tty 2>/dev/null)"
TTY
sudo chmod 0755 /tmp/e2e-tty.sh
out=$(as_h script -qec /tmp/e2e-tty.sh /dev/null | tr -d '\r')
[[ $out =~ caller=(/dev/pts/[0-9]+)\ persona=(/dev/pts/[0-9]+) && ${BASH_REMATCH[1]} != "${BASH_REMATCH[2]}" ]] ||
  fail "persona not on its own terminal (sudo use_pty?): $out"
ok "persona runs on its own terminal: no keystrokes into the caller's shell"

# --- Exit status and signals -------------------------------------------
set +e
as_h chaperone -- sh -c 'exit 7'; rc=$?
[[ $rc == 7 ]] || fail "exit status $rc, want 7"
as_h chaperone -- sleep 30 & pid=$!
for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done
pgrep -u "$P" -x sleep >/dev/null || fail "persona sleep never started"
sudo pkill -TERM -f '^/usr/local/bin/chaperone _enter'
wait "$pid"; rc=$?
set -e
[[ $rc == 143 ]] || fail "SIGTERM: exit $rc, want 143"
pgrep -u "$P" >/dev/null && fail "persona processes survived SIGTERM"
ok "exit status and SIGTERM propagate; the session ends cleanly"

# --- Fail closed: never fall back to the human ---------------------------
sudo usermod -d /tmp "$P"
expect_refusal "persona HOME is not" chaperone -- touch /tmp/e2e-ran
sudo usermod -d "$PH" "$P"
expect_refusal "unknown persona" chaperone -p nope -- touch /tmp/e2e-ran
expect_refusal "invalid _enter arguments" sudo -n /usr/local/bin/chaperone _enter --persona ai --bind rw:../x -- touch /tmp/e2e-ran
expect_refusal "sudo" /usr/local/bin/chaperone _enter --persona ai -- touch /tmp/e2e-ran
# Removed in the directory: the broker refuses at once, also on a direct sudo call.
sudo touch /etc/e2e-ineligible
expect_refusal "not eligible" chaperone -- touch /tmp/e2e-ran
expect_refusal "not eligible" sudo -n /usr/local/bin/chaperone _enter --persona ai -- touch /tmp/e2e-ran
sudo rm /etc/e2e-ineligible
ok "every refusal fails closed without running the command"

# --- Policy change applies on the next launch ------------------------------
site "restricted, lab"
playbook install.yml >/dev/null
[[ $(as_h chaperone -- id -Gn | sorted) == $(echo "$P $TG" | sorted) ]] || fail "newly banned group kept: $(as_h chaperone -- id -Gn)"
[[ $(cat "/tmp/e2e-ipa/$P") == "$TG" ]] || fail "broker kept a newly banned group in the directory"
ok "a newly banned group is gone at the next launch, in the session and the directory"
site "restricted, lab" "[]"
playbook install.yml >/dev/null
as_h sh -c 'echo "export CHAP_X=1" > .bash_login; chmod 600 .bash_login'
as_h chaperone -- true
[[ $(sudo stat -c '%a %G' "$HH/.bash_login") == "600 $H" ]] || fail "startup_files: [] still shared: $(sudo stat -c '%a %G' "$HH/.bash_login")"
ok "startup_files: [] shares no startup file"
site "restricted, lab" "[]" ", list_home: true"
playbook install.yml >/dev/null
[[ $(as_h chaperone -- sh -c 'ls -a . | grep -qx private.txt && echo listed') == listed ]] || fail "list_home: HOME not listed"
[[ $(stat -c '%a %G' "$HH") == "750 $TG" ]] || fail "list_home: HOME $(stat -c '%a %G' "$HH")"
for c in "cat private.txt" "cat .ssh/id" "touch new.txt"; do as_h chaperone -- $c 2>/dev/null && fail "list_home: persona could: $c"; done
ok "list_home: the persona lists HOME (0750); each entry's own mode still decides, HOME stays unwritable"
sudo groupadd e2e-gone && sudo usermod -aG e2e-gone "$H"
[[ $(as_h chaperone -- id -Gn | sorted) == $(echo "$P e2e-gone $TG" | sorted) ]] || fail "new group not in the session: $(as_h chaperone -- id -Gn)"
sudo groupdel e2e-gone
[[ $(as_h chaperone -- id -Gn | sorted) == $(echo "$P $TG" | sorted) ]] || fail "launch after a group deletion: $(as_h chaperone -- id -Gn 2>&1)"
ok "a deleted group is gone at the next launch, which still works"

# --- Retire (default uninstall) --------------------------------------------
running() { as_h chaperone -- sleep 60 & pid=$!; for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done; }
running
playbook uninstall.yml
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived retire"
for p in /etc/sudoers.d/chaperone /usr/local/bin/chaperone /etc/chaperone /var/lib/chaperone; do
  sudo test -e "$p" && fail "left behind after retire: $p"
done
getent passwd chaperone >/dev/null && fail "left behind after retire: service user"
[[ $(stat -c %U /srv/scratch/out.txt) == "$P" ]] || fail "retire changed file ownership"
as_h chaperone -- true 2>/dev/null && fail "launch still possible after retire"
[[ $(stat -c '%a %G' "$HH") == "$home_mode $H" ]] || fail "retire did not give HOME back: $(stat -c '%a %G' "$HH")"
sudo test -d "$PH" || fail "retire removed the persona's HOME"
playbook uninstall.yml | grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' || fail "second retire changed something"
ok "retire stops launches, sessions and persona processes, removes the software, keeps files"

# --- Install again: the retired persona comes back -------------------------
playbook install.yml >/dev/null
[[ $(as_h chaperone -- sh -c 'cat ~/mine') == h ]] || fail "persona lost its HOME across retire"
ok "reinstall after retire: same persona, same files"

# --- Purge -------------------------------------------------------------------
running
playbook uninstall.yml -e chaperone_purge=true
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived purge"
for p in /etc/sudoers.d/chaperone /usr/local/bin/chaperone /etc/chaperone /var/lib/chaperone; do
  sudo test -e "$p" && fail "left behind: $p"
done
getent passwd chaperone >/dev/null && fail "left behind: service user"
[[ $(stat -c '%a %G' "$HH") == "$home_mode $H" ]] || fail "HOME not given back: $(stat -c '%a %G' "$HH"), was $home_mode $H"
[[ $(sudo stat -c '%a %G' "$HH/.bash_profile") == "600 $H" ]] || fail "startup file not given back: $(sudo stat -c '%a %G' "$HH/.bash_profile")"
sudo test -e "$HH/.chaperone" && fail "purge left the personas' HOMEs"
owned=$(sudo find "$HH" -user "$P" -o -group "$P" -o -group "$TG")
[[ -z $owned ]] || fail "persona owns files in HOME: $owned"
[[ -z $(sudo getfacl -R -s -p "$HH" 2>/dev/null) ]] || fail "ACLs in HOME: $(sudo getfacl -R -s -p "$HH")"
ok "purge stops everything; HOME holds only the human's files, no ACLs"

rm -f "$REPO/chaperone.sha256"
echo "e2e: all checks passed"
