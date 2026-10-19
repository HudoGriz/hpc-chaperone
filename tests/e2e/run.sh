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

# --- Fixture -------------------------------------------------------------
for g in research agents restricted; do sudo groupadd "$g"; done
sudo useradd -m -U -G research,restricted -s /bin/bash "$H"
sudo setfacl -k "$HH" # runner /home carries default:other::r-x, which would override umask
# Persona: own UID and private group, the human's HOME, restricted groups.
sudo useradd -M -U -d "$HH" -G research,agents -s /usr/sbin/nologin "$P"
sudo install -d -o root -g restricted -m 0750 /srv/restricted
echo patient | sudo tee /srv/restricted/sample.vcf >/dev/null

sudo install -m 0755 -o root -g root "$BIN" /usr/local/bin/chaperone
sudo install -d -m 0755 /etc/chaperone
sudo tee /etc/chaperone/policy.json >/dev/null <<'EOF'
{
  "default_persona": "ai",
  "provision_url": "https://localhost:9443",
  "eligible_groups": ["research"],
  "denied_groups": ["restricted"],
  "shared_paths": [".claude", ".codex"],
  "personas": {
    "ai": {"keep_groups": ["research", "restricted"], "add_groups": ["agents"], "slurm_account": "agents"}
  }
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
printf '#!/bin/sh\n[ "$1" = user-show ] || { echo "unexpected ipa $*" >&2; exit 1; }\nprintf "homedirectory: %s\\ndescription: chaperone:%s\\nmemberof: cn=agents,cn=groups,cn=accounts,dc=e2e\\nmemberof: cn=research,cn=groups,cn=accounts,dc=e2e\\n"\n' "$HH" "$H" | sudo tee "$S/bin/ipa" >/dev/null
printf '#!/bin/sh\ncase "$*" in *show*) echo agents ;; *) echo "unexpected sacctmgr $*" >&2; exit 1 ;; esac\n' | sudo tee "$S/bin/sacctmgr" >/dev/null
sudo chmod 0755 "$S/bin/ipa" "$S/bin/sacctmgr"
sudo useradd -r -M -s /usr/sbin/nologin chaperone
sudo openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj /CN=localhost \
  -addext subjectAltName=DNS:localhost,IP:127.0.0.1 -keyout "$S/tls.key" -out "$S/tls.crt" 2>/dev/null
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
groups=$(as_h chaperone -- id -Gn | tr ' ' '\n' | sort | xargs)
[[ $groups == "agents $P research" ]] || fail "persona groups: $groups"
ok "groups = keep ∩ human + add, denied dropped: $groups"
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
err=$(as_h chaperone -- true 2>&1) || fail "relaunch with persona-owned files failed: $err"
[[ -z $err ]] || fail "relaunch warned: $err"
ok "relaunch with persona-owned files is clean"

# --- Isolation -----------------------------------------------------------
as_h sh -c 'umask 077; echo secret > private.txt'
[[ $(as_h stat -c %a private.txt) == 600 ]] || fail "fixture: private.txt is not 0600"
if as_h chaperone -- cat private.txt 2>/dev/null; then
  sudo getfacl -p "$HH" "$HH/private.txt"; as_h chaperone -- id; fail "persona read private HOME file"
fi
as_h chaperone -- touch newfile 2>/dev/null && fail "persona wrote into HOME root"
as_h chaperone -- cat /srv/restricted/sample.vcf 2>/dev/null && fail "persona read denied-group data"
as_h cat /srv/restricted/sample.vcf >/dev/null || fail "fixture: human should read restricted data"
ok "persona cannot read private files, write HOME root or denied-group data"

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
sudo gpasswd -d "$P" agents
expect_refusal "missing persona group agents" chaperone -- touch /tmp/e2e-ran
sudo gpasswd -a "$P" agents
sudo gpasswd -a "$P" restricted
expect_refusal "unexpected persona group restricted" chaperone -- touch /tmp/e2e-ran
sudo gpasswd -d "$P" restricted
sudo usermod -d /tmp "$P"
expect_refusal "HOME differs" chaperone -- touch /tmp/e2e-ran
sudo usermod -d "$HH" "$P"
expect_refusal "unknown persona" chaperone -p nope -- touch /tmp/e2e-ran
expect_refusal "sudo" /usr/local/bin/chaperone _enter --persona ai --path /usr/bin -- touch /tmp/e2e-ran
sudo gpasswd -d "$H" research
expect_refusal "not eligible" chaperone -- touch /tmp/e2e-ran
expect_refusal "password" sudo -n /usr/local/bin/chaperone _enter --persona ai --path /usr/bin -- touch /tmp/e2e-ran
sudo gpasswd -a "$H" research
ok "every refusal fails closed without running the command"

echo "e2e: all checks passed"
