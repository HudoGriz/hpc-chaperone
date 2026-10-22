#!/usr/bin/env bash
# Group-only feasibility study (docs/feasibility). Plain Unix groups, chgrp,
# chmod and setgid, a real single-node Slurm, no ACLs, no mounts, no Chaperone.
# Local users stand in for FreeIPA users (the kernel and Slurm see the same
# UIDs and GIDs either way).
#
# Mutates users, groups and /home: run only in a throwaway container:
#   docker run --rm --hostname feas1 --tmpfs /srv/t04m/tree/mnt \
#     -v "$PWD/tests/feasibility:/t:ro" debian:trixie bash /t/run.sh
# (Slurm >= 24.05 for CgroupPlugin=disabled in an unprivileged container.)
# Optional: -v <claude binary>:/opt/claude:ro adds T05 checks with the real
# Claude Code binary (it runs offline, not logged in).
#
# Every line `RESULT|ID|STATUS|requirement` states one requirement:
# PASS = met, FAIL = not met (the detail line shows what happened),
# NOT_TESTED = no fixture here. FAIL is a finding, not a script error.
set -uo pipefail
[ -f /.dockerenv ] || [ "${CHAPERONE_TEST_ISOLATED:-}" = yes ] || { echo "throwaway container only" >&2; exit 2; }
[ "$(id -u)" = 0 ] || { echo "run as root" >&2; exit 2; }
cd /

if ! command -v slurmctld >/dev/null || ! command -v python3 >/dev/null; then
  apt-get -qq update && DEBIAN_FRONTEND=noninteractive apt-get -qq install -y \
    sudo acl strace munge slurm-wlm python3-minimal procps util-linux >/dev/null
fi

PASS=0 FAIL=0 NT=0
res() { # ID STATUS requirement [detail]
  echo "RESULT|$1|$2|$3"
  [ -n "${4:-}" ] && printf '        %s\n' "$(printf '%s' "$4" | head -c 300 | tr '\n' ' ')"
  case $2 in PASS) PASS=$((PASS + 1)) ;; FAIL) FAIL=$((FAIL + 1)) ;; *) NT=$((NT + 1)) ;; esac
}
can() { # ID requirement cmd...  (requirement: cmd succeeds)
  local id=$1 req=$2 out; shift 2
  if out=$("$@" 2>&1); then res "$id" PASS "$req"; else res "$id" FAIL "$req" "$out"; fi
}
cannot() { # ID requirement cmd...  (requirement: cmd fails)
  local id=$1 req=$2 out; shift 2
  if out=$("$@" 2>&1); then res "$id" FAIL "blocked: $req" "succeeded: $out"; else res "$id" PASS "blocked: $req" "$out"; fi
}
# Run as a user with NSS groups (initgroups), HOME from passwd, a fixed umask.
# The persona umask 007 stands for what a launcher would set.
as() { local u=$1; shift; sudo -H -u "$u" -- sh -c 'umask "$0"; exec "$@"' "${UMASK:-022}" "$@"; }
aip() { UMASK=007 as "$@"; } # any persona
ai() { aip alice-ai "$@"; }
mode() { stat -c '%a %U:%G' "$@"; }

# --- Fixture: humans, personas, groups ------------------------------------
for g in projA projB ref admins editors clinical alice-home alice-share bob-share; do groupadd "$g"; done
useradd -m -U -s /bin/bash -G projA,projB,clinical alice
useradd -m -U -s /bin/bash -G projA bob
useradd -m -U -s /bin/bash carol
# Persona: own UID and group, the human's HOME path, only approved groups.
useradd -M -U -d /home/alice -s /bin/bash -G alice-home,alice-share,projA alice-ai
useradd -M -U -d /home/bob -s /bin/bash -G bob-share,projA bob-ai
usermod -aG alice-home,alice-share alice
usermod -aG bob-share bob
chmod 0700 /home/alice /home/bob /home/carol # EL9 HOME_MODE

echo "== T01 separate UID, shared HOME"
[ "$(id -u alice)" != "$(id -u alice-ai)" ] && res T01.1 PASS "different UIDs ($(id -u alice) / $(id -u alice-ai))" || res T01.1 FAIL "different UIDs"
[ "$(getent passwd alice | cut -d: -f6)" = "$(getent passwd alice-ai | cut -d: -f6)" ] && res T01.2 PASS "identical HOME path" || res T01.2 FAIL "identical HOME path"
g=$(id -Gn alice-ai)
case " $g " in *" clinical "*|*" admins "*|*" editors "*|*" projB "*|*" alice "*) res T01.3 FAIL "only approved groups" "$g" ;; *) res T01.3 PASS "only approved groups: $g" ;; esac
echo hi > /home/alice/.bashrc; chown alice:alice /home/alice/.bashrc; chmod 0644 /home/alice/.bashrc
# HOME's group becomes a dedicated traverse group. The human's private group
# would not do: membership would open every group-readable file of the human.
chgrp alice-home /home/alice
for m in 0700 0710 0750 0770; do
  chmod "$m" /home/alice
  t=no l=no w=no
  ai cat /home/alice/.bashrc >/dev/null 2>&1 && t=yes
  ai ls /home/alice >/dev/null 2>&1 && l=yes
  ai touch /home/alice/.probe 2>/dev/null && w=yes && rm -f /home/alice/.probe
  res "T01.mode$m" INFO "HOME $m alice:alice-home" "traverse=$t list=$l write=$w"
  [ $t = yes ] && [ $l = no ] && [ $w = no ] && ok_modes="${ok_modes:-}$m "
done
[ "${ok_modes:-}" = "0710 " ] && res T01.5 PASS "smallest secure HOME mode: 0710 with a dedicated traverse group (traverse, no list, no write)" || res T01.5 FAIL "HOME mode" "${ok_modes:-none}"
chmod 0710 /home/alice
can T01.4 "human HOME group change is the only HOME edit (mode 0710, group alice-home)" test "$(mode /home/alice)" = "710 alice:alice-home"

echo "== T02 shell startup and command execution"
H=/home/alice
install -d -o alice -g alice -m 0755 $H/.local $H/.local/bin $H/.config
cat > $H/.bash_profile <<'EOF'
[ -f ~/.bashrc ] && . ~/.bashrc
export PROFILE_SEEN=1
EOF
cat > $H/.bashrc <<'EOF'
export PATH="$HOME/.local/bin:$PATH" AI_VAR=from-bashrc
id -u > "/tmp/bashrc-ran-by-$(id -u)"
[ -r ~/.config/private-env.sh ] && . ~/.config/private-env.sh
EOF
echo 'export PRIVATE_VAR=1' > $H/.config/private-env.sh
printf '#!/bin/sh\nprintf "[%%s]" "$@"; echo; exit 7\n' > $H/.local/bin/argv
chown alice:alice $H/.bash_profile $H/.bashrc $H/.config/private-env.sh $H/.local/bin/argv
chmod 0644 $H/.bash_profile $H/.bashrc; chmod 0600 $H/.config/private-env.sh; chmod 0755 $H/.local/bin/argv
out=$(sudo -iu alice-ai env 2>&1)
case $out in *AI_VAR=from-bashrc*PROFILE_SEEN=1*|*PROFILE_SEEN=1*AI_VAR=from-bashrc*) res T02.1 PASS "login shell (sudo -iu) reads the human's .bash_profile and .bashrc" ;; *) res T02.1 FAIL "login shell reads startup files" "$out" ;; esac
case $out in *"PATH=/home/alice/.local/bin:"*) res T02.2 PASS "PATH from the human's .bashrc" ;; *) res T02.2 FAIL "PATH from the human's .bashrc" "$out" ;; esac
case $out in *PRIVATE_VAR*) res T02.3 FAIL "nested 0600 file stays private" ;; *) res T02.3 PASS "nested 0600 file stays private (persona env lacks PRIVATE_VAR: such files must be shared to match the human)" ;; esac
can T02.4 ".bashrc ran as the persona, not as root or the human" test "$(cat /tmp/bashrc-ran-by-"$(id -u alice-ai)" 2>&1)" = "$(id -u alice-ai)"
test ! -e /tmp/bashrc-ran-by-0 && res T02.5 PASS "no startup file ran as root" || res T02.5 FAIL "no startup file ran as root"
out=$(sudo -u alice-ai -H bash -ic 'echo "$AI_VAR"' 2>/dev/null)
[ "$out" = from-bashrc ] && res T02.6 PASS "interactive non-login shell reads .bashrc" || res T02.6 FAIL "interactive non-login shell reads .bashrc" "$out"
out=$(sudo -u alice-ai -H bash -c 'echo "${AI_VAR:-unset}"' 2>&1)
[ "$out" = unset ] && res T02.7 PASS "non-interactive shell reads no startup file (same as for the human)" || res T02.7 FAIL "non-interactive shell" "$out"
out=$(sudo -iu alice-ai argv "argument with spaces" 'b"q' 2>&1); rc=$?
[ "$out" = '[argument with spaces][b"q]' ] && [ $rc = 7 ] && res T02.8 PASS "user-installed program via persona PATH: argv boundaries and exit code kept" || res T02.8 FAIL "argv boundaries and exit code" "rc=$rc out=$out"
can T02.9 "python --version as the persona" sudo -iu alice-ai python3 --version
res T02.10 NOT_TESTED "environment modules (Lmod) initialisation" "no Lmod in this fixture; /etc/profile.d runs for login shells as above"

echo "== T03 non-recursive sharing of a directory"
P=$H/proj
install -d -o alice -g alice -m 0755 $P
for m in 0600 0640 0644 0660; do echo data > $P/f$m; chown alice:alice $P/f$m; chmod $m $P/f$m; done
echo data > $P/fprojA; chown alice:projA $P/fprojA; chmod 0640 $P/fprojA
for m in 0700 0750 0770; do install -d -o alice -g alice -m $m $P/d$m; done
# include PATH: the named directory only.
as alice sh -c "chgrp alice-share $P && chmod 2770 $P"
can T03.1 "persona lists and enters the shared root" ai ls $P
can T03.2 "persona reads existing descendant 0640 (group alice)" ai cat $P/f0640
can T03.3 "persona writes existing descendant 0660 (group alice)" ai sh -c "echo x >> $P/f0660"
can T03.4 "persona enters existing subdirectory 0750 (group alice)" ai ls $P/d0750
can T03.5 "persona reads existing 0640 file of another group it holds (projA)" ai cat $P/fprojA
ai sh -c "echo new > $P/by-ai; mkdir $P/dir-by-ai"
can T03.6 "new persona file inherits the share group (setgid)" test "$(stat -c %G $P/by-ai)" = alice-share
can T03.7 "human edits persona-created file (persona umask 007)" as alice sh -c "echo y >> $P/by-ai"
as alice sh -c "echo new > $P/by-human; mkdir $P/dir-by-human"
can T03.8 "persona edits a file the human creates later (human umask 022)" ai sh -c "echo z >> $P/by-human"
can T03.9 "persona creates files in a subdirectory the human creates later (umask 022)" ai touch $P/dir-by-human/x
UMASK=002 as alice sh -c "echo new > $P/by-human-002; mkdir $P/dir-002"
can T03.10 "...same with human umask 002" ai sh -c "echo z >> $P/by-human-002 && touch $P/dir-002/x"
as alice python3 -c "import os; os.close(os.open('$P/explicit0600', os.O_CREAT|os.O_WRONLY, 0o600))"
can T03.11 "persona reads a new file the human's tool creates with explicit mode 0600" ai cat $P/explicit0600
as alice sh -c "echo moved > $H/outside; mv $H/outside $P/moved-in"
can T03.12 "persona writes a file the human moves in (rename keeps the old group)" ai sh -c "echo w >> $P/moved-in"

echo "== T04 explicit recursive sharing (include -R prototype)"
# Prototype only: preflight refusal, journal, chgrp/chmod g+rwX, setgid dirs.
include_r() { # root group journal budget
  local root=$1 grp=$2 j=$3 budget=$4 n bad
  n=$(find -P "$root" -xdev | wc -l)
  [ "$n" -le "$budget" ] || { echo "refuse: $n entries > budget $budget"; return 3; }
  findmnt -rno TARGET | grep -q "^$root/" && { echo "refuse: mount point inside"; return 3; }
  bad=$(find -P "$root" -xdev \( ! -user "$(id -un)" -o -group clinical -o -group admins -o -group editors \) -print -quit)
  [ -z "$bad" ] || { echo "refuse: $bad"; return 3; }
  # ponytail: one line per path, names with newlines unsupported; NUL journal if this ships.
  find -P "$root" -xdev ! -type l -printf '%m %g %p\n' > "$j"
  find -P "$root" -xdev ! -type l -exec chgrp "$grp" {} + -exec chmod g+rwX {} + &&
    find -P "$root" -xdev -type d -exec chmod g+s {} +
}
restore() { while read -r m g p; do chgrp "$g" "$p" && chmod "00$m" "$p"; done < "$1"; }
T=/srv/t04/tree; J=/srv/t04/journal
mkdir -p $T/a/b $T/c; for i in $(seq 1 16); do echo d > $T/a/b/f$i; done
echo secret > /srv/t04/outside; ln -s /srv/t04/outside $T/link-out
chmod 0600 $T/a/b/f1; chmod 0700 $T/c
chown -R alice:alice /srv/t04; chmod 0755 /srv/t04
if findmnt -rno TARGET | grep -q '^/srv/t04m/tree/mnt$'; then
  chown alice:alice /srv/t04m /srv/t04m/tree
  out=$(as alice sh -c "$(declare -f include_r); include_r /srv/t04m/tree alice-share /srv/t04m/j 999" 2>&1)
  case $out in *"mount point inside"*) res T04.1 PASS "refuses a tree that crosses a filesystem boundary" ;; *) res T04.1 FAIL "refuses a mount inside the tree" "$out" ;; esac
else res T04.1 NOT_TESTED "filesystem boundary" "run with --tmpfs /srv/t04m/tree/mnt"; fi
before=$(find -P $T -printf '%m %g %p\n' | sort)
out=$(as alice sh -c "$(declare -f include_r); include_r $T alice-share $J 5" 2>&1)
[ "$out" != "${out#refuse: }" ] && [ "$before" = "$(find -P $T -printf '%m %g %p\n' | sort)" ] && res T04.2 PASS "operation budget: preview count refuses a large tree, nothing changed" "$out" || res T04.2 FAIL "budget refusal" "$out"
install -d -o alice -g clinical -m 0750 $T/c/clin
out=$(as alice sh -c "$(declare -f include_r); include_r $T alice-share $J 999" 2>&1)
case $out in *"refuse: $T/c/clin"*) res T04.3 PASS "protected group inside the tree: refused before any change" ;; *) res T04.3 FAIL "protected group refusal" "$out" ;; esac
rmdir $T/c/clin
before=$(find -P $T -printf '%m %g %p\n' | sort)
strace -f -c -o /srv/t04/strace.txt sudo -H -u alice -- sh -c "$(declare -f include_r); include_r $T alice-share $J 999"; rc=$?
[ $rc = 0 ] && res T04.4 PASS "include -R exits 0 on full success" || res T04.4 FAIL "include -R" "rc=$rc"
can T04.5 "persona reads existing 0600 descendant after include -R" ai cat $T/a/b/f1
can T04.6 "persona enters existing 0700 subdirectory after include -R" ai ls $T/c
can T04.7 "symlink target outside the tree untouched" test "$(mode /srv/t04/outside)" = "644 alice:alice"
entries=$(find -P $T | wc -l)
ops=$(awk '$NF ~ /^(fchownat|fchmodat|chown|chmod|lchown|fchown|fchmod|newfstatat|statx|getdents64|openat)$/ {s+=$4} END {print s}' /srv/t04/strace.txt)
res T04.8 PASS "metadata syscalls: $ops for $entries entries (~$((ops / entries)) per entry, incl. sudo/sh overhead)"
as alice sh -c "$(declare -f restore); restore $J"
test -s $J && [ "$before" = "$(find -P $T -printf '%m %g %p\n' | sort)" ] && res T04.9 PASS "journal restores every mode and group exactly" || res T04.9 FAIL "journal restore" "$(diff <(echo "$before") <(find -P $T -printf '%m %g %p\n' | sort))"
chown bob $T/a/b/f2
out=$(as alice sh -c "$(declare -f include_r); include_r $T alice-share $J.y 999" 2>&1); rc=$?
[ $rc != 0 ] && [ "$before" = "$(find -P $T -printf '%m %g %p\n' | sort)" ] && res T04.10 PASS "foreign-owned descendant: refused, nonzero exit, nothing changed" || res T04.10 FAIL "foreign-owned refusal" "rc=$rc $out"

echo "== T05 Claude/Codex credentials"
C=$H/.claude
install -d -o alice -g alice-share -m 2770 $C
# Claude Code 2.1.306 secureStorage.write: atomic write with mode 0600, then
# chmod(path, 0o600) (see FEASIBILITY.md). Reproduced with the same syscalls:
claude_write() { umask 077; printf '%s' "$2" > "$1.tmp.$$" && mv -f "$1.tmp.$$" "$1" && chmod 600 "$1"; }
as alice sh -c "$(declare -f claude_write); claude_write $C/.credentials.json human-login"
can T05.1 "persona reads the human's Claude login (as Claude writes it: 0600)" ai cat $C/.credentials.json
chmod 0660 $C/.credentials.json # an include of the file
can T05.2 "persona reads it after include (0660 alice-share)" ai cat $C/.credentials.json
ai sh -c "$(declare -f claude_write); claude_write $C/.credentials.json persona-refresh"
res T05.3 INFO "after persona token refresh: $(mode $C/.credentials.json)"
can T05.4 "human still reads the login after the persona refreshes it" as alice cat $C/.credentials.json
can T05.5 "human can repair it without root (owner is now the persona)" as alice chmod 0660 $C/.credentials.json
can T05.6 "HOME stays non-writable for the persona" test "$(mode $H)" = "710 alice:alice-home"
# Codex 0.163.0 FileAuthStorage::save: open(create|truncate|write, mode 0600):
# in place, so owner and mode survive a refresh; mode applies only on create.
as alice sh -c "umask 077; mkdir -p $H/.codex; echo human > $H/.codex/auth.json"
as alice sh -c "chgrp -R alice-share $H/.codex && chmod 2770 $H/.codex && chmod 0660 $H/.codex/auth.json"
ai sh -c "echo persona-refresh > $H/.codex/auth.json"
can T05.7 "Codex: human reads auth.json after a persona refresh (owner stays human, 0660)" test "$(mode $H/.codex/auth.json) $(as alice cat $H/.codex/auth.json)" = "660 alice:alice-share persona-refresh"
as alice sh -c "echo human-refresh > $H/.codex/auth.json"
can T05.8 "Codex: persona reads auth.json after a human refresh" ai cat $H/.codex/auth.json
as alice sh -c "rm $H/.codex/auth.json; umask 022; python3 -c \"import os; os.close(os.open('$H/.codex/auth.json', os.O_CREAT|os.O_WRONLY|os.O_TRUNC, 0o600))\""
can T05.9 "Codex: persona reads auth.json after the human logs out and in again" ai cat $H/.codex/auth.json
# Long-lived token (claude setup-token): one read-only file, nothing rewrites it.
as alice sh -c "umask 077; echo token > $H/.config/claude-token && chgrp alice-share $H/.config/claude-token && chmod 0640 $H/.config/claude-token"
can T05.10 "relaxation: persona reads a long-lived token file (CLAUDE_CODE_OAUTH_TOKEN)" ai cat $H/.config/claude-token
if [ -x /opt/claude ]; then
  run_claude() { sudo -H -u "$1" -- env CLAUDE_CONFIG_DIR=$C DISABLE_TELEMETRY=1 CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 sh -c "umask $2; timeout 60 /opt/claude -p hi </dev/null" >/dev/null 2>&1; }
  # A minimal .claude.json makes Claude rewrite it at startup, offline and logged out.
  as alice sh -c "echo '{\"numStartups\":1}' > $C/.claude.json && chmod 0660 $C/.claude.json"
  run_claude alice-ai 007
  res T05.11 INFO "real Claude: shared .claude.json (alice, 0660) after a persona run: $(mode $C/.claude.json)"
  can T05.12 "real Claude: human still writes .claude.json after a persona rewrite" as alice test -w $C/.claude.json
  can T05.13 "real Claude: human keeps ownership of .claude.json" test "$(stat -c %U $C/.claude.json)" = alice
  cannot T05.14 "real Claude: persona cannot lock the human out of .claude.json" ai chmod 0600 $C/.claude.json
else
  res T05.11 NOT_TESTED "real Claude binary" "mount it at /opt/claude"
fi

echo "== T06 excluded groups"
cannot T06.1 "persona joins a banned group with newgrp/sg" ai sg clinical -c true
echo x > /tmp/ai-file; chown alice-ai:alice-ai /tmp/ai-file
cannot T06.2 "persona chgrps its own file to a banned group" ai chgrp clinical /tmp/ai-file
install -d -o alice -g clinical -m 0750 $H/clin; echo patient > $H/clin/sample; chown alice:clinical $H/clin/sample; chmod 0640 $H/clin/sample
cannot T06.3 "persona reads data protected by a banned group" ai cat $H/clin/sample
as alice sh -c "chgrp alice-share $H/clin $H/clin/sample"
cannot T06.4 "human (owner) cannot re-group banned data to a share group outside Chaperone" ai cat $H/clin/sample
install -d -o root -g clinical -m 2770 /srv/clinical; echo patient > /srv/clinical/s; chown alice:clinical /srv/clinical/s; chmod 0660 /srv/clinical/s
as alice sh -c "cp /srv/clinical/s $P/copied"
cannot T06.5 "banned-group data copied by the human into a share stays closed" ai cat $P/copied
res T06.6 NOT_TESTED "FreeIPA nested groups re-adding a banned group" "local groups do not nest; see FEASIBILITY.md"

echo "== T07 two humans, one shared project"
install -d -o root -g projA -m 2770 /srv/projA; echo a > /srv/projA/data; chgrp projA /srv/projA/data; chmod 0660 /srv/projA/data
gA=$(stat -c %g /srv/projA)
ai sh -c "echo by-alice-ai > /srv/projA/by-alice-ai"
aip bob-ai sh -c "echo by-bob-ai > /srv/projA/by-bob-ai"
can T07.1 "both personas work in an existing project group" test -s /srv/projA/by-alice-ai -a -s /srv/projA/by-bob-ai
can T07.2 "project GID unchanged" test "$(stat -c %g /srv/projA)" = "$gA"
can T07.3 "human collaborator edits a persona file" as bob sh -c "echo e >> /srv/projA/by-alice-ai"
gpasswd -d alice-ai projA >/dev/null
can T07.4 "removing one persona leaves the other working" aip bob-ai sh -c "echo f >> /srv/projA/by-alice-ai"
cannot T07.5 "removed persona loses access (new session)" ai cat /srv/projA/data
usermod -aG projA alice-ai
install -d -o bob -g bob-share -m 2770 /home/bob/share; chmod 0710 /home/bob; chgrp bob-share /home/bob
echo b > /home/bob/share/x; chgrp bob-share /home/bob/share/x; chmod 0660 /home/bob/share/x
cannot T07.6 "alice-ai cannot reach bob's own share" ai cat /home/bob/share/x
install -d -o alice -g projA -m 2770 /srv/projA/sub; echo s > /srv/projA/sub/s; chown alice:projA /srv/projA/sub/s; chmod 0660 /srv/projA/sub/s
as alice chgrp -R alice-share /srv/projA/sub
can T07.7 "sharing one subdir through a new group keeps project members' access" as bob cat /srv/projA/sub/s
as alice chgrp -R projA /srv/projA/sub

echo "== T08 one folder for two projects"
install -d -o root -g projB -m 2770 /srv/projB; echo secretB > /srv/projB/secret; chgrp projB /srv/projB/secret; chmod 0660 /srv/projB/secret
install -d -o root -g projA -m 2770 /srv/shared-ref; echo ref > /srv/shared-ref/r; chgrp projA /srv/shared-ref/r; chmod 0640 /srv/shared-ref/r
useradd -M -U -d /home/alice -s /bin/bash -G alice-home,projB alice-ai-b
can T08.1 "projB persona reads shared-reference owned by projA" aip alice-ai-b cat /srv/shared-ref/r
chgrp -R ref /srv/shared-ref; usermod -aG ref alice-ai-b
can T08.2 "dedicated resource group: projB persona reads it" aip alice-ai-b cat /srv/shared-ref/r
can T08.3 "dedicated resource group: projA humans keep access without joining it" as bob cat /srv/shared-ref/r
cannot T08.4 "distinct personas: projB persona cannot read projA" aip alice-ai-b cat /srv/projA/data
chgrp -R projA /srv/shared-ref

echo "== T09 one persona, two projects"
usermod -aG projB alice-ai
cannot T09.1 "persona started for projA cannot read projB" ai sh -c "cd /srv/projA && cat /srv/projB/secret"
gidA=$(getent group projA | cut -d: -f3); uid=$(id -u alice-ai)
narrow() { setpriv --reuid "$uid" --regid "$(id -g alice-ai)" --groups "$gidA,$(getent group alice-home | cut -d: -f3)" env HOME=$H "$@"; }
cannot T09.2 "launcher narrows groups to projA (setgroups): projB closed" narrow cat /srv/projB/secret
cannot T09.3 "narrowed persona cannot regain projB with sg (member, no password)" narrow sg projB -c "cat /srv/projB/secret"

echo "== Slurm (single node, no accounting)"
mungekey -c -f >/dev/null 2>&1 || true; chown munge: /etc/munge/munge.key; install -d -o munge -g munge /run/munge
sudo -u munge munged
N=$(hostname)
cat > /etc/slurm/slurm.conf <<EOF
ClusterName=feas
SlurmctldHost=$N
SlurmUser=root
AuthType=auth/munge
ProctrackType=proctrack/linuxproc
TaskPlugin=task/none
JobAcctGatherType=jobacct_gather/none
AccountingStorageType=accounting_storage/none
StateSaveLocation=/var/spool/slurmctld
SlurmdSpoolDir=/var/spool/slurmd
ReturnToService=2
$(slurmd -C | head -1 | sed 's/ UpTime.*//')
PartitionName=debug Nodes=ALL Default=YES MaxTime=INFINITE State=UP
EOF
echo CgroupPlugin=disabled > /etc/slurm/cgroup.conf
mkdir -p /var/spool/slurmctld /var/spool/slurmd /var/log/slurm
slurmctld; slurmd
for _ in $(seq 30); do sinfo -h -o %T 2>/dev/null | grep -q idle && break; sleep 1; done
if sinfo -h -o %T 2>/dev/null | grep -q idle; then
  out=$(narrow sbatch --wait -Q -D /srv/projA -o /srv/projA/narrow.out --wrap "id; cat /srv/projB/secret" 2>&1)
  if grep -q secretB /srv/projA/narrow.out 2>/dev/null; then res T09.4 FAIL "batch job from a projA-narrowed launch cannot read projB" "$(cat /srv/projA/narrow.out)"; else res T09.4 PASS "batch job from a narrowed launch cannot read projB" "$out $(cat /srv/projA/narrow.out 2>&1)"; fi
  gpasswd -d alice-ai projB >/dev/null

  echo "== T11 native Slurm as the persona"
  out=$(ai sbatch --wait -Q -D $H -o $P/job.out --wrap 'id -u; id -Gn; cat ~/.bashrc >/dev/null && echo home-ok; echo job > job-file' 2>&1)
  j=$(cat $P/job.out 2>&1)
  [ "$(echo "$j" | head -1)" = "$uid" ] && res T11.1 PASS "sbatch runs as the persona UID" || res T11.1 FAIL "sbatch UID" "$out $j"
  # bob-ai: groups unchanged since Slurm first saw it (alice-ai's are cached, T12.7).
  aip bob-ai sbatch --wait -Q -D /srv/projA -o /srv/projA/bob.out --wrap 'id -Gn' >/dev/null 2>&1
  [ "$(tr ' ' '\n' < /srv/projA/bob.out | sort | tr '\n' ' ')" = "$(id -Gn bob-ai | tr ' ' '\n' | sort | tr '\n' ' ')" ] && res T11.2 PASS "job supplementary groups = NSS groups of the persona" || res T11.2 FAIL "job groups" "$(cat /srv/projA/bob.out 2>&1)"
  case $j in *home-ok*) res T11.3 PASS "job with workdir = shared HOME (0710) reads startup files; stdout in a shared dir" ;; *) res T11.3 FAIL "job in HOME" "$j" ;; esac
  cannot T11.4 "job cannot create files in HOME itself" test -e $H/job-file
  out=$(ai sbatch --wait -Q -D $P -o $P/job2.out --wrap 'echo job > job-file' 2>&1)
  can T11.5 "job-created file in a shared dir: share group, human can edit" sh -c "test \"\$(stat -c %G $P/job-file)\" = alice-share && sudo -u alice sh -c 'echo h >> $P/job-file'"
  out=$(ai srun -D /srv/projA id -u 2>&1); [ "$out" = "$uid" ] && res T11.6 PASS "srun as the persona" || res T11.6 FAIL "srun" "$out"
  out=$(ai salloc -Q srun id -u 2>&1); [ "$(echo "$out" | tail -1)" = "$uid" ] && res T11.7 PASS "salloc + srun as the persona" || res T11.7 FAIL "salloc" "$out"
  out=$(ai sbatch --wait -Q -a 1-3 -D $P -o "$P/arr-%a.out" --wrap 'id -u' 2>&1)
  [ "$(cat $P/arr-*.out 2>/dev/null | sort -u)" = "$uid" ] && [ "$(ls $P/arr-*.out | wc -l)" = 3 ] && res T11.8 PASS "job array as the persona" || res T11.8 FAIL "array" "$out"
  out=$(ai sbatch --wait -Q -D $P -o $P/login.out --wrap 'bash -lc "echo \$AI_VAR"' 2>&1)
  [ "$(cat $P/login.out)" = from-bashrc ] && res T11.9 PASS "login shell in a job reads the human's .bashrc" || res T11.9 FAIL "login shell in job" "$(cat $P/login.out 2>&1)"
  res T11.10 NOT_TESTED "several node types, SSSD on compute nodes" "single container node"

  echo "== T12 revocation"
  usermod -aG projB alice-ai
  ai sbatch -Q -D /srv/projA -o /srv/projA/revoke.out --wrap 'sleep 4; cat /srv/projB/secret' >/dev/null 2>&1
  sleep 1; gpasswd -d alice-ai projB >/dev/null; sleep 6
  out=$(ai sbatch --wait -Q -D /srv/projA -o /srv/projA/revoke2.out --wrap 'cat /srv/projB/secret' 2>&1)
  if grep -q secretB /srv/projA/revoke2.out 2>/dev/null; then
    t0=$(date +%s) gone=no
    while [ $(($(date +%s) - t0)) -lt 600 ]; do
      sleep 15
      ai sbatch --wait -Q -D /srv/projA -o /srv/projA/revoke3.out --wrap 'cat /srv/projB/secret' >/dev/null 2>&1
      grep -q secretB /srv/projA/revoke3.out 2>/dev/null || { gone=yes; break; }
    done
    res T12.7 FAIL "new job after a group removal has no access" "Slurm reused its cached group list; still cached after $(($(date +%s) - t0)) s more (gone=$gone)"
  else res T12.7 PASS "new job after a group removal has no access"; fi
  grep -q secretB /srv/projA/revoke.out 2>/dev/null && res T12.1 FAIL "running job loses a group removed while it runs" "job still read projB after removal" || res T12.1 PASS "running job revoked" "$(cat /srv/projA/revoke.out 2>&1)"
else
  res T09.4 NOT_TESTED "Slurm" "slurmd did not reach idle: $(tail -3 /var/log/slurm/*.log 2>/dev/null)"
  res T11.1 NOT_TESTED "Slurm" ""
  gpasswd -d alice-ai projB >/dev/null
fi
usermod -aG projB alice-ai
ai sleep 30 & sleep 1
pid=$(pgrep -u alice-ai -x sleep | head -1)
gpasswd -d alice-ai projB >/dev/null
grep -q "$(getent group projB | cut -d: -f3)" <(grep ^Groups: /proc/"$pid"/status) && res T12.2 FAIL "running interactive session loses a removed group" "$(grep ^Groups: /proc/$pid/status)" || res T12.2 PASS "running session revoked"
kill "$pid" 2>/dev/null
cannot T12.3 "new session after removal has no access" ai cat /srv/projB/secret
usermod -aG projB alice-ai
can T12.4 "group added before launch is effective at once (no cache here)" ai cat /srv/projB/secret
gpasswd -d alice-ai projB >/dev/null
res T12.5 NOT_TESTED "SSSD cache delay" "no SSSD; FreeIPA sites need sss_cache -E or wait entry_cache_timeout"
userdel alice-ai-b
can T12.6 "human can chmod a file the persona created (e.g. to clean up after retirement)" as alice chmod 0600 $P/by-ai

echo "== T10 unrelated users"
cannot T10.1 "persona reads a 0700 HOME of another user" ai ls /home/carol
chmod 0755 /home/carol; echo pub > /home/carol/notes; chown carol: /home/carol/notes; chmod 0644 /home/carol/notes
cannot T10.2 "persona reads another user's world-readable file (strict allowlist)" ai cat /home/carol/notes
ln -s /home/carol/notes $P/link-to-carol
cannot T10.3 "persona follows a symlink from a share to another user's readable file" ai cat $P/link-to-carol
install -d -o carol -g carol -m 0700 /home/carol/priv; echo p > /home/carol/priv/p; ln -s /home/carol/priv/p $P/link-priv
cannot T10.4 "symlink to another user's private file" ai cat $P/link-priv
as alice sh -c "echo mine > $H/unshared-0644"
cannot T10.5 "persona reads an unshared 0644 file of its human (HOME 0710, name known)" ai cat $H/unshared-0644

echo "== T14 cost"
install -d -o alice -g alice -m 0755 $H/big; for i in $(seq 1 1000); do : > $H/big/f$i; done; chown -R alice:alice $H/big
as alice strace -f -c -o /tmp/inc.txt sh -c "chgrp alice-share $H/big && chmod 2770 $H/big"
d=$(awk '$NF=="getdents64"{print $4}' /tmp/inc.txt)
[ -z "$d" ] && res T14.1 PASS "non-recursive include of a 1000-entry dir reads no directory entries" || res T14.1 FAIL "non-recursive include" "getdents64 calls: $d"
[ -z "$(getfacl -Rsp /home /srv 2>/dev/null)" ] && res T14.2 PASS "no ACL anywhere after all tests" || res T14.2 FAIL "ACLs" "$(getfacl -Rsp /home /srv | head)"
[ "$(findmnt -rno TARGET | grep -E '^/(home|srv)' | grep -vc '^/srv/t04m/')" = 0 ] && res T14.3 PASS "no mounts under /home or /srv" || res T14.3 FAIL "mounts"

echo "== T15 privilege escalation"
cannot T15.1 "persona chmods a human file" ai chmod 0666 $P/f0644
cannot T15.2 "persona chgrps a human file" ai chgrp alice-share $P/f0644
cannot T15.3 "persona edits the human's .bashrc" ai sh -c "echo evil >> $H/.bashrc"
cannot T15.4 "persona creates a file in HOME (e.g. .bash_login)" ai touch $H/.bash_login
as alice sh -c "echo '{}' > $C/settings.json; chmod 0644 $C/settings.json"
cannot T15.5 "persona replaces the human's read-only settings.json (hooks) in a shared dir" ai sh -c "echo '{\"hooks\":\"evil\"}' > $C/s.tmp && mv -f $C/s.tmp $C/settings.json"
chmod +t $C; as alice sh -c "echo '{}' > $C/settings2.json; chmod 0644 $C/settings2.json; rm -f $C/.claude.json; echo x > $C/.claude.json; chmod 0660 $C/.claude.json" 2>/dev/null
cannot T15.6 "sticky shared dir: persona replaces settings2.json" ai sh -c "echo e > $C/s2.tmp && mv -f $C/s2.tmp $C/settings2.json"
can T15.7 "sticky shared dir: persona can still rename-update .claude.json as Claude does" ai sh -c "echo u > $C/cj.tmp && mv -f $C/cj.tmp $C/.claude.json"
chmod -t $C
cannot T15.8 "a collaborator re-groups a persona file in a shared project" as bob chgrp bob-share /srv/projA/by-alice-ai
cannot T15.9 "persona edits its own group memberships" ai gpasswd -a alice-ai clinical
res T15.10 NOT_TESTED "job submitted to another Slurm account" "no accounting here; associations are covered by tests/integration"

echo "== SUMMARY pass=$PASS fail=$FAIL not_tested/info=$NT"
