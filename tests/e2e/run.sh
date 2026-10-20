#!/usr/bin/env bash
# End-to-end on a disposable CI runner: the real install.yml and
# uninstall.yml (login-node plays), real UIDs, sudo, ACLs, the broker as its
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
for g in chaperone-users restricted; do sudo groupadd "$g"; done
sudo useradd -m -U -G chaperone-users,restricted -s /bin/bash "$H"
sudo chmod 0700 "$HH"  # as FreeIPA's mkhomedir makes it: the persona must still get through
sudo setfacl -k "$HH"  # runner /home carries default:other::r-x, which would override umask
home_mode=$(stat -c %a "$HH") # uninstall must leave HOME's mode as it was
# Personas: own UID, own group (the human a member), the human's HOME.
for p in "$P" "$R"; do
  sudo useradd -M -U -d "$HH" -s /usr/sbin/nologin "$p"
  sudo usermod -aG "$p" "$H"
done
sudo install -d -o root -g restricted -m 0750 /srv/restricted
echo patient | sudo tee /srv/restricted/sample.vcf >/dev/null
sudo install -d -m 0777 /srv/scratch # world-writable, not sticky
as_h sh -c 'umask 077; echo "export CHAP_LOGIN=from-bash-profile" > .bash_profile; echo "[user]" > .gitconfig; echo secret > private.txt'

# Stubs the broker calls (PATH puts /usr/local/bin first). ipa reports the
# personas as already provisioned and logs who asked.
stub() { sudo tee "/usr/local/bin/$1" >/dev/null; sudo chmod 0755 "/usr/local/bin/$1"; }
gid() { getent group "$1" | cut -d: -f3; }
stub ipa <<EOF
#!/bin/sh
id -un >> /tmp/e2e-broker-calls
case "\$1:\$2" in
group-show:$P) printf 'description: chaperone:$H\\ngidnumber: $(gid "$P")\\nmember: uid=$H,cn=users,dc=e2e\\n' ;;
group-show:$R) printf 'description: chaperone:$H\\ngidnumber: $(gid "$R")\\nmember: uid=$H,cn=users,dc=e2e\\n' ;;
user-show:$P) printf 'homedirectory: $HH\\nuserclass: chaperone:$H\\ngidnumber: $(gid "$P")\\nmemberof: cn=ipausers,cn=groups,dc=e2e\\n' ;;
user-show:$R) printf 'homedirectory: $HH\\nuserclass: chaperone:$H\\ngidnumber: $(gid "$R")\\nmemberof: cn=ipausers,cn=groups,dc=e2e\\n' ;;
user-show:$H) [ -e /etc/e2e-ineligible ] || echo 'memberof: cn=chaperone-users,cn=groups,dc=e2e' ;;
user-show:*) echo "ipa: ERROR: \$2: user not found"; exit 2 ;;
*) echo "unexpected ipa \$*" >&2; exit 1 ;;
esac
EOF
printf '#!/bin/sh\ncase "$*" in *show*) echo agents ;; *) echo "unexpected sacctmgr $*" >&2; exit 1 ;; esac\n' | stub sacctmgr
printf '#!/bin/sh\ncase "$*" in *MEMORY:check*) test -s /etc/chaperone/chaperone.keytab ;; esac\n' | stub kinit
printf '#!/bin/sh\nwhile [ $# -gt 0 ]; do [ "$1" = -k ] && echo stub > "$2"; shift; done\n' | stub ipa-getkeytab
printf '#!/bin/sh\nexit 0\n' | stub kdestroy

# --- Install with the real playbook ---------------------------------------
(cd "$REPO" && sha256sum chaperone deploy/* > chaperone.sha256)
[[ $BIN == "$REPO/chaperone" ]] || fail "run from a build in the repository root"
printf '[chaperone]\nlocalhost ansible_connection=local chaperone_fqdn=e2e.test\n[chaperone_ipa]\n[chaperone_slurmdbd]\n' > "$T/inventory"
site() { # $1: write paths of profile ai
  cat > "$T/site.yml" <<EOF
chaperone_policy:
  default_persona: ai
  eligible_groups: [chaperone-users]
  personas:
    ai: {slurm_account: agents, list_home: true, workdir: auto, login: true, write: [$1], read: [.bash_profile, .gitconfig]}
    ai-restrictive: {slurm_account: agents, write: [.claude]}
EOF
}
site ".claude, .codex"
playbook install.yml
playbook install.yml | tee "$T/second.log"
grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' "$T/second.log" || fail "second install changed something"
ok "install.yml installs, and a second run changes nothing"

# --- Identity --------------------------------------------------------------
[[ $(as_h chaperone -- id -un) == "$P" ]] || fail "command did not run as $P"
[[ $(as_h chaperone -p ai-restrictive -- id -un) == "$R" ]] || fail "profile did not select $R"
grep -qx chaperone /tmp/e2e-broker-calls || fail "broker did not run as the service user"
grep -qx root /tmp/e2e-broker-calls && fail "broker ran as root"
ok "runs as the profile's persona; the broker runs as its service user, never root"
[[ $(as_h env https_proxy=http://proxy.e2e:3128 ANTHROPIC_BASE_URL=x chaperone -- sh -c 'echo $https_proxy $ANTHROPIC_BASE_URL' 2>/dev/null) == "http://proxy.e2e:3128 x" ]] || fail "proxy/agent env did not reach the persona"
ok "site proxy and agent settings reach the persona"
[[ $(as_h chaperone -- id -Gn) == "$P" ]] || fail "persona groups: $(as_h chaperone -- id -Gn)"
ok "persona holds only its own group"
as_h chaperone -- sh -c 'echo p > /srv/scratch/out.txt'
[[ $(stat -c %a:%G /srv/scratch/out.txt) == "660:$P" ]] || fail "persona file outside shared dirs: $(stat -c %a:%G /srv/scratch/out.txt)"
as_h sh -c 'echo h >> /srv/scratch/out.txt' || fail "human cannot write a persona file through the persona group"
ok "human controls persona files anywhere through the persona's group"

# --- Profiles: HOME, startup files, read paths -----------------------------
[[ $(as_h chaperone -- sh -c 'echo $CHAP_LOGIN') == from-bash-profile ]] || fail "login profile did not source .bash_profile"
[[ -z $(as_h chaperone -p ai-restrictive -- sh -c 'echo $CHAP_LOGIN') ]] || fail "restrictive profile sourced startup files"
as_h chaperone -- ls "$HH" | grep -qx private.txt || fail "list_home: persona cannot list HOME"
as_h chaperone -p ai-restrictive -- ls "$HH" >/dev/null 2>&1 && fail "restrictive persona listed HOME"
[[ $(as_h chaperone -- cat .gitconfig) == "[user]" ]] || fail "read path: persona cannot read a 0600 .gitconfig"
as_h chaperone -p ai-restrictive -- cat .gitconfig 2>/dev/null && fail "restrictive persona read .gitconfig"
as_h chaperone -- sh -c 'echo x >> .gitconfig' 2>/dev/null && fail "persona wrote a read path"
ok "profiles: ai lists HOME, sources startup files, reads its read paths; ai-restrictive does none of it"

# --- Shared writable state ----------------------------------------------
# Atomic 0600 replace by the persona (how harnesses refresh OAuth tokens).
as_h chaperone -- sh -c 'umask 077; echo p-token > ~/.claude/.c.tmp && mv ~/.claude/.c.tmp ~/.claude/.credentials.json'
[[ $(sudo stat -c %U "$HH/.claude/.credentials.json") == "$P" ]] || fail "expected persona-owned credentials"
[[ $(as_h cat .claude/.credentials.json) == p-token ]] || fail "human locked out of persona-written 0600 file"
ok "human reads persona's 0600 credentials after session"
as_h sh -c 'umask 077; mkdir -p .codex && echo h-token > .codex/auth.json'
[[ $(as_h chaperone -- cat .codex/auth.json) == h-token ]] || fail "persona locked out of human-written 0600 file"
ok "persona reads human's 0600 credentials on next launch"
as_h chaperone -- sh -c 'umask 077; mkdir -p ~/.claude/projects/p && echo s > ~/.claude/projects/p/s.jsonl'
[[ $(as_h cat .claude/projects/p/s.jsonl) == s ]] || fail "human cannot read persona's 0700 directory"
ok "persona 0700 directories handed back"
as_h sh -c 'echo "{\"theme\":\"dark\"}" > .claude.json; rm -f .claude/.claude.json'
[[ $(as_h chaperone -- sh -c 'cat "$CLAUDE_CONFIG_DIR/.claude.json"') == '{"theme":"dark"}' ]] || fail "persona Claude config not seeded into the shared dir"
ok "persona Claude Code config seeded from the human's"

# --- Work directory: wherever the human launches, files stay editable both ways
W=$HH/work/proj
as_h sh -c 'mkdir -p work/proj && echo human-code > work/proj/main.go'
as_hin "$W" chaperone -- sh -c 'echo persona-edit >> main.go && echo persona-new > new.txt && mkdir -p out && echo r > out/result.csv'
[[ $(as_hin "$W" cat main.go) == $'human-code\npersona-edit' ]] || fail "persona could not edit human's existing file"
as_hin "$W" sh -c 'echo human-edit >> new.txt && echo h >> out/result.csv && rm out/result.csv' || fail "human cannot edit/delete persona's files"
[[ $(sudo stat -c %U "$W/new.txt") == "$P" ]] || fail "fixture: new.txt should be persona-owned"
ok "persona and human edit each other's files in a launch directory"
err=$(as_hin "$W" chaperone -- true 2>&1) || fail "relaunch with persona-owned files failed: $err"
[[ -z $err ]] || fail "relaunch warned: $err"
ok "relaunch with persona-owned files is clean"
[[ $(as_h chaperone -- true 2>&1) == *"HOME or its ancestor"* ]] || fail "HOME launch did not explain HOME is not shared"
ok "launch from HOME shares nothing beyond the profile"

# --- Isolation -----------------------------------------------------------
as_h chaperone -- cat private.txt 2>/dev/null && fail "persona read a private (0600) HOME file"
as_h chaperone -- touch newfile 2>/dev/null && fail "persona wrote into HOME root"
as_h chaperone -- cat /srv/restricted/sample.vcf 2>/dev/null && fail "persona read the human's group data"
as_h cat /srv/restricted/sample.vcf >/dev/null || fail "fixture: human should read restricted data"
ok "persona cannot read private HOME files, write HOME root or the human's group data"
as_h chaperone -- sudo -n true >/dev/null 2>&1 && fail "persona ran sudo"
ok "no_new_privs: no setuid program (sudo included) works for the persona"
# use_pty: the persona gets a terminal of its own, so it cannot push
# keystrokes (TIOCSTI) into the caller's shell. Sudo before 1.9.14 (EL9)
# does not do this by default.
sudo tee /tmp/e2e-tty.sh >/dev/null <<'EOF'
#!/bin/bash
cd
echo "caller=$(tty) persona=$(chaperone -- tty 2>/dev/null)"
EOF
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
ok "exit status and SIGTERM propagate"

# --- Fail closed: never fall back to the human ---------------------------
expect_refusal() { # want-substring, then command
  local want=$1; shift
  rm -f /tmp/e2e-ran
  if out=$(as_h "$@" 2>&1); then fail "accepted: $* ($out)"; fi
  [[ $out == *"$want"* ]] || fail "wrong refusal for $*: $out"
  [[ ! -e /tmp/e2e-ran ]] || fail "command ran despite refusal: $*"
}
sudo gpasswd -a "$P" restricted
expect_refusal "unexpected persona group restricted" chaperone -- touch /tmp/e2e-ran
sudo gpasswd -d "$P" restricted
sudo usermod -d /tmp "$P"
expect_refusal "HOME differs" chaperone -- touch /tmp/e2e-ran
sudo usermod -d "$HH" "$P"
expect_refusal "unknown persona" chaperone -p nope -- touch /tmp/e2e-ran
expect_refusal "sudo" /usr/local/bin/chaperone _enter --persona ai --path /usr/bin -- touch /tmp/e2e-ran
# Removed in the directory: the broker refuses at once, also on a direct sudo call.
sudo touch /etc/e2e-ineligible
expect_refusal "not eligible" chaperone -- touch /tmp/e2e-ran
expect_refusal "not eligible" sudo -n /usr/local/bin/chaperone _enter --persona ai --path /usr/bin -- touch /tmp/e2e-ran
sudo rm /etc/e2e-ineligible
ok "every refusal fails closed without running the command"

# --- Policy change applies on the next launch ------------------------------
site ".claude"
playbook install.yml >/dev/null
as_h chaperone -- true 2>/dev/null
sudo getfacl -P -p "$HH/.codex" | grep -E "user:$P:|default:user:$P:" && fail ".codex still shared after leaving the policy"
as_h chaperone -- cat .codex/auth.json 2>/dev/null && fail "persona still reads a revoked path"
ok "a path removed from the policy is revoked at the next launch"

# --- Retire (default uninstall): everything stops, nothing is walked -------
sudo grep -q "$P" /var/lib/chaperone/grants || fail "grants not recorded"
running() { as_h chaperone -- sleep 60 & pid=$!; for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done; }
running
playbook uninstall.yml
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived retire"
for p in /etc/sudoers.d/chaperone /usr/local/bin/chaperone /etc/chaperone /var/lib/chaperone/broker; do
  sudo test -e "$p" && fail "left behind after retire: $p"
done
getent passwd chaperone >/dev/null && fail "left behind after retire: service user"
sudo test -s /var/lib/chaperone/grants || fail "retire dropped the grant record a later purge needs"
[[ $(sudo stat -c %U "$W/new.txt") == "$P" ]] || fail "retire changed file ownership"
sudo getfacl -P -p "$HH/.claude" | grep -q "user:$P:" || fail "retire walked the shared tree"
as_h chaperone -- true 2>/dev/null && fail "launch still possible after retire"
playbook uninstall.yml | grep -Eq 'localhost +: ok=[0-9]+ +changed=0 ' || fail "second retire changed something"
ok "retire stops launches and persona processes, removes the software, keeps files, ACLs and the record"

# --- Reinstall fixture: retained files do not require a migration ----------
playbook install.yml >/dev/null
[[ $(as_hin "$W" tail -n 1 new.txt) == human-edit ]] || fail "retire lost the retained file"
ok "retire keeps files; real IPA-disabled identities require explicit admin reactivation"

# --- Purge: nothing of the persona left ------------------------------------
running
playbook uninstall.yml -e chaperone_purge=true
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived uninstall"
for p in /etc/sudoers.d/chaperone /usr/local/bin/chaperone /etc/chaperone /var/lib/chaperone; do
  sudo test -e "$p" && fail "left behind: $p"
done
getent passwd chaperone >/dev/null && fail "left behind: service user"
left=$(sudo getfacl -R -P -p "$HH/.claude" "$HH/.codex" "$HH/work" "$HH/.gitconfig" "$HH/.bash_profile" 2>/dev/null | grep -E "user:($P|$R|$H):" || true)
[[ -z $left ]] || fail "ACL entries left: $left"
left=$(sudo getfacl -R -P -s -p "$HH/.claude" "$HH/.codex" "$HH/work" "$HH/.gitconfig" 2>/dev/null)
[[ -z $left ]] || fail "ACLs left (none existed before): $left"
sudo getfacl -n -P -p "$HH" | grep -E "user:($(id -u "$P")|$(id -u "$R")):" && fail "persona entries left on HOME"
[[ $(stat -c %a "$HH") == "$home_mode" ]] || fail "HOME mode changed: $(stat -c %a "$HH"), was $home_mode"
owned=$(sudo find "$HH" -user "$P" -o -user "$R")
[[ -z $owned ]] || fail "persona still owns: $owned"
[[ $(as_hin "$W" cat new.txt) == $'persona-new\nhuman-edit' ]] || fail "human lost a persona file"
# Documented limit: files outside recorded trees stay with the persona's UID.
[[ $(stat -c %U /srv/scratch/out.txt) == "$P" ]] || fail "expected the scratch file to stay persona-owned (documented)"
ok "purge stops persona processes, hands files back, removes every ACL entry and file"

rm -f "$REPO/chaperone.sha256"
echo "e2e: all checks passed"
