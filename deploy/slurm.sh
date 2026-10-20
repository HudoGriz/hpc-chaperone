#!/bin/sh
# Chaperone's Slurm objects. Run as a Slurm administrator (root on the
# slurmdbd/controller host):
#
#   slurm.sh install   SERVICE_USER ACCOUNT...
#   slurm.sh uninstall SERVICE_USER ACCOUNT...
#
# install: each persona account (no limits: add your own) with SERVICE_USER,
# the broker's Unix user, as its coordinator. Idempotent.
# uninstall: cancels the accounts' jobs, removes their users (completely when
# they have no other account) and the accounts. Job history stays.
set -eu
action=${1:?} svc=${2:?}
shift 2
has() { [ -n "$(sacctmgr -n -P "$@")" ]; }
case $action in
install)
  for a; do
    has show account "$a" format=Account || sacctmgr -i add account "$a" description="Chaperone personas"
    has show assoc where account="$a" user="$svc" format=User || sacctmgr -i add user "$svc" account="$a"
    sacctmgr -n -P show account "$a" withcoord format=Coordinators | tr , '\n' | grep -qx "$svc" ||
      sacctmgr -i add coordinator account="$a" names="$svc"
  done
  ;;
uninstall)
  accounts=""
  for a; do has show account "$a" format=Account && accounts="$accounts $a"; done
  [ -n "$accounts" ] || exit 0
  for a in $accounts; do scancel -A "$a" || true; done
  for _ in $(seq 60); do
    busy=""
    for a in $accounts; do busy="$busy$(squeue -h -A "$a")"; done
    [ -z "$busy" ] && break
    sleep 2
  done
  list=$(echo $accounts | tr ' ' ,)
  for u in $(sacctmgr -n -P show assoc where account="$list" format=User | sort -u); do
    others=$(sacctmgr -n -P show assoc where user="$u" format=Account | sort -u | grep -vxF -e "$(echo $accounts | tr ' ' '\n')" || true)
    if [ -z "$others" ]; then
      sacctmgr -i delete user name="$u"
    else
      sacctmgr -i delete user name="$u" account="$list"
    fi
  done
  sacctmgr -i delete account name="$list"
  ;;
*) echo "usage: slurm.sh install|uninstall SERVICE_USER ACCOUNT..." >&2; exit 2 ;;
esac
