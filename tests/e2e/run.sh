#!/usr/bin/env bash
# End-to-end launch with two real UIDs on a disposable CI runner:
# client -> MUNGE (stub) -> HTTPS broker (stub) -> setfacl -> sudo _enter -> persona.
# Local users stand in for FreeIPA; tests/integration covers real IPA/Slurm.
# Mutates users, groups, sudoers and CA trust: GitHub Actions only.
set -Eeuo pipefail
: "${GITHUB_ACTIONS:?disposable CI runner only}"
: "${RUNNER_TEMP:?}"
BIN=$(realpath "${1:?usage: run.sh path/to/chaperone}")
H=chaperone-e2e
P=$H-ai
HH=/home/$H
T=$RUNNER_TEMP/chaperone-e2e
mkdir -p "$T"

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }
as_h() { sudo -H -u "$H" sh -c 'cd && exec "$@"' sh "$@"; }
as_hin() { local d=$1; shift; sudo -H -u "$H" sh -c 'cd "$1" && shift && exec "$@"' sh "$d" "$@"; }

# --- Fixture -------------------------------------------------------------
for g in chaperone-users restricted; do sudo groupadd "$g"; done
sudo useradd -m -U -G chaperone-users,restricted -s /bin/bash "$H"
sudo setfacl -k "$HH" # runner /home carries default:other::r-x, which would override umask
home_acl=$(sudo getfacl -P -p "$HH") # the runner adds its own entries; uninstall must restore exactly this
# Persona: own UID, its own group (the human a member), the human's HOME.
sudo useradd -M -U -d "$HH" -s /usr/sbin/nologin "$P"
sudo usermod -aG "$P" "$H"
PGID=$(getent group "$P" | cut -d: -f3)
sudo install -d -o root -g restricted -m 0750 /srv/restricted
echo patient | sudo tee /srv/restricted/sample.vcf >/dev/null
sudo install -d -m 0777 /srv/scratch # world-writable, not sticky

sudo install -m 0755 -o root -g root "$BIN" /usr/local/bin/chaperone
sudo install -d -m 0755 /etc/chaperone
sudo tee /etc/chaperone/policy.json >/dev/null <<'EOF'
{
  "default_persona": "ai",
  "provision_url": "https://localhost:9443",
  "eligible_groups": ["chaperone-users"],
  "shared_paths": [".claude", ".codex"],
  "personas": {"ai": {"slurm_account": "agents"}}
}
EOF
sudo tee /etc/sudoers.d/chaperone >/dev/null < "$(dirname "$0")/../../deploy/chaperone.sudoers"
sudo chmod 0440 /etc/sudoers.d/chaperone
sudo visudo -cf /etc/sudoers.d/chaperone

# Real MUNGE and the real broker as an unprivileged service user. Only the
# IPA/Slurm CLIs are stubs: they report the persona as already provisioned.
sudo apt-get -qq update && sudo apt-get -qq install -y munge >/dev/null
sudo test -s /etc/munge/munge.key || sudo mungekey -c
sudo systemctl restart munge
S=/opt/chaperone-e2e
sudo install -d -m 0755 "$S/bin"
sudo tee "$S/bin/ipa" >/dev/null <<EOF
#!/bin/sh
case "\$1" in
group-show) printf 'description: chaperone:$H\\ngidnumber: $PGID\\nmember: uid=$H,cn=users,dc=e2e\\n' ;;
user-show) case "\$2" in
  $P) printf 'homedirectory: $HH\\nuserclass: chaperone:$H\\ngidnumber: $PGID\\nmemberof: cn=ipausers,cn=groups,dc=e2e\\n' ;;
  $H) [ -e /etc/e2e-ineligible ] || echo 'memberof: cn=chaperone-users,cn=groups,dc=e2e' ;;
  *) echo "ipa: ERROR: \$2: user not found"; exit 2 ;;
  esac ;;
*) echo "unexpected ipa \$*" >&2; exit 1 ;;
esac
EOF
printf '#!/bin/sh\ncase "$*" in *show*) echo agents ;; *) echo "unexpected sacctmgr $*" >&2; exit 1 ;; esac\n' | sudo tee "$S/bin/sacctmgr" >/dev/null
sudo chmod 0755 "$S/bin/ipa" "$S/bin/sacctmgr"
sudo useradd -r -M -s /usr/sbin/nologin chaperone
sudo openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=localhost \
  -addext subjectAltName=DNS:localhost,IP:127.0.0.1 \
  -addext 'nameConstraints=critical,permitted;DNS:localhost,permitted;IP:127.0.0.1/255.255.255.255' \
  -keyout "$S/tls.key" -out "$S/tls.crt" 2>/dev/null
sudo chown chaperone "$S/tls.key"
sudo cp "$S/tls.crt" /usr/local/share/ca-certificates/chaperone-e2e.crt
sudo update-ca-certificates >/dev/null
sudo -u chaperone env PATH="$S/bin:/usr/bin:/bin" \
  /usr/local/bin/chaperone provisiond --listen 127.0.0.1:9443 --cert "$S/tls.crt" --key "$S/tls.key" 2>"$T/broker.log" &
trap 'sudo pkill -f "chaperone provisiond" || true; cat "$T/broker.log"' EXIT
for _ in $(seq 50); do ss -ltn | grep -q ':9443 ' && break; sleep 0.1; done
out=$(sudo /usr/local/bin/chaperone provisiond --listen 127.0.0.1:1 --cert x --key y 2>&1 || true)
[[ $out == *"not root"* ]] || fail "provisiond accepted root: $out"

# --- Identity ------------------------------------------------------------
[[ $(as_h chaperone -- id -un) == "$P" ]] || fail "command did not run as $P"
ok "runs as persona"
[[ $(as_h env https_proxy=http://proxy.e2e:3128 ANTHROPIC_BASE_URL=x chaperone -- sh -c 'echo $https_proxy $ANTHROPIC_BASE_URL' 2>/dev/null) == "http://proxy.e2e:3128 x" ]] || fail "proxy/agent env did not reach the persona"
ok "site proxy and agent settings reach the persona"
groups=$(as_h chaperone -- id -Gn)
[[ $groups == "$P" ]] || fail "persona groups: $groups"
ok "persona holds only its own group"
as_h chaperone -- sh -c 'echo p > /srv/scratch/out.txt'
[[ $(stat -c %a:%G /srv/scratch/out.txt) == "660:$P" ]] || fail "persona file outside shared dirs: $(stat -c %a:%G /srv/scratch/out.txt)"
as_h sh -c 'echo h >> /srv/scratch/out.txt' || fail "human cannot write a persona file through the persona group"
ok "human controls persona files anywhere through the persona's group"
grep -q "provisioned human=\"$H\" profile=\"ai\" persona=\"$P\"" "$T/broker.log" || fail "broker did not provision via MUNGE"
ok "real MUNGE credential verified by real broker"
post() { curl -s -o /dev/null -w '%{http_code}' --noproxy '*' -H 'Content-Type: application/json' \
  -d "{\"credential\":\"$1\"}" https://localhost:9443/v1/provision; }
cred=$(as_h sh -c 'printf chaperone:v1:ai | munge')
[[ $(post "$cred") == 200 ]] || fail "fresh credential rejected"
[[ $(post "$cred") == 401 ]] || fail "replayed MUNGE credential accepted"
[[ $(post "$(as_h sh -c 'printf chaperone:v1:nope | munge')") == 403 ]] || fail "unknown persona accepted"
[[ $(post "$(sudo -u nobody sh -c 'printf chaperone:v1:ai | munge')") == 403 ]] || fail "ineligible UID accepted"
[[ $(post "${cred%?}x") == 401 ]] || fail "forged credential accepted"
ok "broker rejects replay, forgery, unknown persona and ineligible UID"

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
[[ $(as_h chaperone -- true 2>&1) == *"run from a project directory"* ]] || fail "HOME launch did not explain HOME is not shared"
ok "launch from HOME shares nothing beyond agent state"

# --- Isolation -----------------------------------------------------------
as_h sh -c 'umask 077; echo secret > private.txt'
[[ $(as_h stat -c %a private.txt) == 600 ]] || fail "fixture: private.txt is not 0600"
if as_h chaperone -- cat private.txt 2>/dev/null; then
  sudo getfacl -p "$HH" "$HH/private.txt"; as_h chaperone -- id; fail "persona read private HOME file"
fi
as_h chaperone -- touch newfile 2>/dev/null && fail "persona wrote into HOME root"
as_h chaperone -- cat /srv/restricted/sample.vcf 2>/dev/null && fail "persona read the human's group data"
as_h cat /srv/restricted/sample.vcf >/dev/null || fail "fixture: human should read restricted data"
ok "persona cannot read private HOME files, write HOME root or the human's group data"

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
# Removed in the directory (the broker reads IPA, stubbed here) and in NSS:
# refused at once, by the broker and by _enter on a direct sudo call.
sudo touch /etc/e2e-ineligible
expect_refusal "ineligible" chaperone -- touch /tmp/e2e-ran
sudo rm /etc/e2e-ineligible
sudo gpasswd -d "$H" chaperone-users
expect_refusal "no longer eligible" chaperone -- touch /tmp/e2e-ran
expect_refusal "no longer eligible" sudo -n /usr/local/bin/chaperone _enter --persona ai --path /usr/bin -- touch /tmp/e2e-ran
sudo gpasswd -a "$H" chaperone-users
ok "every refusal fails closed without running the command"

# --- Uninstall: nothing of the persona left on the human's files -----------
sudo grep -q "$P" /var/log/chaperone-launches || fail "launches not logged"
as_h chaperone -- sleep 60 & pid=$!
for _ in $(seq 100); do pgrep -u "$P" -x sleep >/dev/null && break; sleep 0.1; done
sudo rm /etc/sudoers.d/chaperone
sudo /usr/local/bin/chaperone _uninstall || fail "_uninstall failed"
wait "$pid" || true
pgrep -u "$P" >/dev/null && fail "persona processes survived _uninstall"
left=$(sudo getfacl -R -P -p "$HH/.claude" "$HH/.codex" "$HH/work" 2>/dev/null | grep -E "user:($P|$H):" || true)
[[ -z $left ]] || fail "ACL entries left: $left"
left=$(sudo getfacl -R -P -s -p "$HH/.claude" "$HH/.codex" "$HH/work" 2>/dev/null)
[[ -z $left ]] || fail "ACLs left (none existed before): $left"
home_diff=$(diff <(echo "$home_acl") <(echo "$(sudo getfacl -P -p "$HH")")) || fail "HOME ACL not restored (< before, > after): $home_diff"
owned=$(sudo find "$HH" -user "$P")
[[ -z $owned ]] || fail "persona still owns: $owned"
[[ $(as_hin "$W" cat new.txt) == $'persona-new\nhuman-edit' ]] || fail "human lost a persona file"
ok "_uninstall stops persona processes, hands its files to the human, removes every ACL entry"

echo "e2e: all checks passed"
