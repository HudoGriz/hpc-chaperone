#!/usr/bin/env bash
# End-to-end on a disposable CI runner: the real install.yml and
# uninstall.yml (login-node plays), real UIDs, sudo, bindfs, the broker as its
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
R=$H-ai-restrictive
HH=/home/$H
T=$RUNNER_TEMP/chaperone-e2e
mkdir -p "$T"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
as_h() { sudo -H -u "$H" sh -c 'cd && exec "$@"' sh "$@"; }
as_hin() { local d=$1; shift; sudo -H -u "$H" sh -c 'cd "$1" && shift && exec "$@"' sh "$d" "$@"; }
playbook() { sudo "$(command -v ansible-playbook)" -i "$T/inventory" -e @"$T/site.yml" "$REPO/deploy/$1" "${@:2}"; }


# --- Fixture: what FreeIPA would hold -----------------------------------
for g in chaperone-users restricted lab; do sudo groupadd "$g"; done
sudo useradd -m -U -G chaperone-users,restricted,lab -s /bin/bash "$H"
sudo chmod 0700 "$HH"  # as FreeIPA's mkhomedir makes it
sudo setfacl -R -b "$HH"  # runner /home has default ACLs, inherited by the skel copy; FreeIPA homes have none
home_mode=$(stat -c %a "$HH") # nothing may change it
# Personas: own UID, own group (the human a member), the human's HOME.
for p in "$P" "$R"; do
  sudo useradd -M -U -d "$HH" -s /usr/sbin/nologin "$p"
  sudo usermod -aG "$p" "$H"
done
for g in restricted lab; do
  sudo install -d -o root -g "$g" -m 0750 "/srv/$g"
  echo "$g-data" | sudo tee "/srv/$g/data" >/dev/null
done
sudo install -d -m 0777 /srv/scratch # world-writable, not sticky
as_h sh -c 'umask 077; echo "export CHAP_LOGIN=from-bash-profile" > .bash_profile; echo secret > private.txt
  mkdir .ssh .claude && echo key > .ssh/id && echo "{}" > .claude/settings.json'

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
group-show:$P|group-show:$R) printf 'description: chaperone:$H\\ngidnumber: %s\\nmember: uid=$H,cn=users,dc=e2e\\n' "\$(gid "\$2")" ;;
user-show:$P|user-show:$R) printf 'homedirectory: $HH\\nuserclass: chaperone:$H\\ngidnumber: %s\\nmemberof: cn=ipausers,cn=groups,dc=e2e\\n' "\$(gid "\$2")"; groups "\$2" ;;
user-show:$H) [ -e /etc/e2e-ineligible ] || printf 'memberof: cn=%s,cn=groups,dc=e2e\\n' chaperone-users restricted lab $P $R ;;
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
sudo apt-get install -y -qq bindfs python3-apt >/dev/null # install.yml's package task then finds it
(cd "$REPO" && sha256sum chaperone deploy/* > chaperone.sha256)
[[ $BIN == "$REPO/chaperone" ]] || fail "run from a build in the repository root"
printf '[chaperone]\nlocalhost ansible_connection=local chaperone_fqdn=e2e.test\n[chaperone_ipa]\n[chaperone_slurmdbd]\n' > "$T/inventory"
site() { # $1: ban_groups of profile ai
  cat > "$T/site.yml" <<SITE
chaperone_uid: 960
chaperone_policy:
  default_persona: ai
  eligible_groups: [chaperone-users]
  personas:
    ai: {slurm_account: agents, shell: /bin/bash, ban_groups: [$1]}
    ai-restrictive: {slurm_account: agents, groups: [], home_readonly: true, writable: [.claude], hidden: [.ssh]}
SITE
}
site restricted
playbook install.yml
playbook install.yml | tee "$T/second.log"
grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' "$T/second.log" || fail "second install changed something"
ok "install.yml installs, and a second run changes nothing"

# --- Identity and groups ---------------------------------------------------
[[ $(as_h chaperone -- id -un) == "$P" ]] || fail "command did not run as $P"
[[ $(as_h chaperone -p ai-restrictive -- id -un) == "$R" ]] || fail "profile did not select $R"
grep -qx chaperone /tmp/e2e-broker-calls || fail "broker did not run as the service user"
grep -qx root /tmp/e2e-broker-calls && fail "broker ran as root"
ok "runs as the profile's persona; the broker runs as its service user, never root"
sorted() { tr ' ' '\n' | sort | xargs; }
[[ $(as_h chaperone -- id -Gn | sorted) == $(echo "$P lab" | sorted) ]] || fail "ai groups: $(as_h chaperone -- id -Gn)"
[[ $(cat "/tmp/e2e-ipa/$P") == lab ]] || fail "broker did not give $P the human's groups in the directory"
[[ $(as_h chaperone -- cat /srv/lab/data) == lab-data ]] || fail "persona cannot read the human's group data"
as_h chaperone -- cat /srv/restricted/data 2>/dev/null && fail "persona read a banned group's data"
[[ $(as_h chaperone -p ai-restrictive -- id -Gn) == "$R" ]] || fail "empty allow list: $(as_h chaperone -p ai-restrictive -- id -Gn)"
ok "persona holds the human's groups minus the ban list (an empty allow list: none)"
[[ $(as_h env https_proxy=http://proxy.e2e:3128 chaperone -- sh -c 'echo "${https_proxy-unset} $HOME"') == "unset $HH" ]] || fail "persona env is the caller's"
[[ $(as_h chaperone -- sh -c 'echo $CHAP_LOGIN') == from-bash-profile ]] || fail "login shell did not source .bash_profile"
[[ -z $(as_h chaperone -p ai-restrictive -- sh -c 'echo $CHAP_LOGIN') ]] || fail "profile without shell sourced startup files"
ok "the persona's environment comes from its login shell, not the caller"

# --- HOME through bindfs: the human's rights, the human's files ------------
[[ $(as_h chaperone -- cat private.txt) == secret ]] || fail "persona cannot read the human's 0600 file"
as_h chaperone -- sh -c 'echo p > new.txt; mkdir -p d/e; umask 077; echo tok > .claude/.c.tmp && mv .claude/.c.tmp .claude/.credentials.json'
for f in new.txt d/e .claude/.credentials.json; do
  [[ $(sudo stat -c %U:%G "$HH/$f") == "$H:$H" ]] || fail "$f is not the human's on disk: $(sudo stat -c %U:%G "$HH/$f")"
done
[[ $(sudo stat -c %a "$HH/.claude/.credentials.json") == 600 && $(as_h cat .claude/.credentials.json) == tok ]] || fail "atomic 0600 replace"
ok "persona reads the human's 0600 files; what it writes, also by atomic replace, is the human's"
[[ $(as_hin "$HH/d" chaperone -- sh -c 'pwd; echo w > here.txt') == "$HH/d" ]] || fail "launch directory in HOME"
[[ $(sudo stat -c %U "$HH/d/here.txt") == "$H" ]] || fail "launch directory write not the human's"
sudo install -d -o "$H" -g "$H" -m 0755 /srv/scratch/proj
as_hin /srv/scratch/proj chaperone -- sh -c 'echo p > /srv/scratch/out.txt'
[[ $(stat -c %a:%G /srv/scratch/out.txt) == "660:$P" ]] || fail "persona file outside HOME: $(stat -c %a:%G /srv/scratch/out.txt)"
as_h sh -c 'echo h >> /srv/scratch/out.txt' || fail "human cannot write a persona file outside HOME"
ok "outside HOME the persona's files stay group-writable for the human"
# Locks reach the real files: the human and the persona see each other's.
as_h flock .lockf sleep 3 &
sleep 1
as_h chaperone -- flock -n .lockf true && fail "persona took a lock the human holds"
wait
ok "file locks are shared between the human and the persona"

# --- Profile layer: read-only HOME, writable exceptions, hidden paths ------
as_h chaperone -p ai-restrictive -- sh -c 'echo x >> .bash_profile' 2>/dev/null && fail "wrote a dotfile in read-only HOME"
as_h chaperone -p ai-restrictive -- touch newdot 2>/dev/null && fail "created a file in read-only HOME"
as_h chaperone -p ai-restrictive -- touch .claude/ok || fail "writable .claude not writable"
[[ $(sudo stat -c %U "$HH/.claude/ok") == "$H" ]] || fail "writable path write not the human's"
[[ -z $(as_h chaperone -p ai-restrictive -- ls -A .ssh) ]] || fail "hidden .ssh shows files"
[[ $(as_h chaperone -p ai-restrictive -- cat private.txt) == secret ]] || fail "read-only HOME not readable"
[[ $(as_h cat .ssh/id) == key ]] || fail "hiding changed the human's view"
ok "home_readonly, writable and hidden apply to the persona only"

# --- Session ends: nothing of it stays -----------------------------------
[[ $(as_h chaperone -- sh -c 'setsid sleep 300 </dev/null >/dev/null 2>&1 & echo started') == started ]] || fail "leftover fixture"
pgrep -u "$P" >/dev/null && fail "leftover persona process survived its session"
pgrep -x bindfs >/dev/null && fail "bindfs left running"
grep -q "$HH" /proc/self/mounts && fail "the human sees a session mount"
[[ -z $(sudo ls -A /run/chaperone) ]] || fail "session directory left: $(sudo ls -A /run/chaperone)"
ok "after a session: no persona process, no bindfs, no mount, no state"
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
pgrep -x bindfs >/dev/null && fail "bindfs survived SIGTERM"
ok "exit status and SIGTERM propagate; the session ends cleanly"

# --- Fail closed: never fall back to the human ---------------------------
expect_refusal() { # want-substring, then command
  local want=$1; shift
  rm -f /tmp/e2e-ran
  if out=$(as_h "$@" 2>&1); then fail "accepted: $* ($out)"; fi
  [[ $out == *"$want"* ]] || fail "wrong refusal for $*: $out"
  [[ ! -e /tmp/e2e-ran ]] || fail "command ran despite refusal: $*"
}
sudo usermod -d /tmp "$P"
expect_refusal "HOME differs" chaperone -- touch /tmp/e2e-ran
sudo usermod -d "$HH" "$P"
expect_refusal "unknown persona" chaperone -p nope -- touch /tmp/e2e-ran
expect_refusal "no shell" chaperone -p ai-restrictive
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
[[ $(as_h chaperone -- id -Gn) == "$P" ]] || fail "newly banned group kept: $(as_h chaperone -- id -Gn)"
[[ -z $(cat "/tmp/e2e-ipa/$P") ]] || fail "broker kept a newly banned group in the directory"
ok "a newly banned group is gone at the next launch, in the session and the directory"

# --- Retire (default uninstall) --------------------------------------------
running() { as_h chaperone -- sleep 60 & pid=$!; for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done; }
running
playbook uninstall.yml
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived retire"
pgrep -x bindfs >/dev/null && fail "bindfs survived retire"
for p in /etc/sudoers.d/chaperone /usr/local/bin/chaperone /etc/chaperone /var/lib/chaperone; do
  sudo test -e "$p" && fail "left behind after retire: $p"
done
getent passwd chaperone >/dev/null && fail "left behind after retire: service user"
[[ $(stat -c %U /srv/scratch/out.txt) == "$P" ]] || fail "retire changed file ownership"
as_h chaperone -- true 2>/dev/null && fail "launch still possible after retire"
playbook uninstall.yml | grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' || fail "second retire changed something"
ok "retire stops launches, sessions and persona processes, removes the software, keeps files"

# --- Install again: the retired persona comes back -------------------------
playbook install.yml >/dev/null
[[ $(as_h chaperone -- cat new.txt) == p ]] || fail "persona lost its files across retire"
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
[[ $(stat -c %a "$HH") == "$home_mode" ]] || fail "HOME mode changed: $(stat -c %a "$HH"), was $home_mode"
owned=$(sudo find "$HH" -user "$P" -o -user "$R" -o -group "$P" -o -group "$R")
[[ -z $owned ]] || fail "persona owns files in HOME: $owned"
[[ -z $(sudo getfacl -R -s -p "$HH" 2>/dev/null) ]] || fail "ACLs in HOME: $(sudo getfacl -R -s -p "$HH")"
ok "purge stops everything; HOME holds only the human's files, no ACLs"

rm -f "$REPO/chaperone.sha256"
echo "e2e: all checks passed"
