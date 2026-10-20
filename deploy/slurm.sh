#!/bin/sh
# Chaperone's Slurm objects. Run as a Slurm administrator (root on the
# slurmdbd/controller host):
#
#   slurm.sh install   SERVICE_USER ACCOUNT...
#   slurm.sh uninstall SERVICE_USER ACCOUNT...
#
# install: each persona account (no limits: add your own) with SERVICE_USER,
# the broker's Unix user, as its coordinator. Idempotent. An existing account
# not made by Chaperone is refused.
# uninstall: cancels the accounts' jobs and fails if any survive, removes
# their users (completely when they have no other account) and the
# accounts. Only accounts marked as Chaperone's are touched. Job history stays.
set -eu
action=${1:?} svc=${2:?}
shift 2
desc="chaperone personas" # sacctmgr stores descriptions lower-cased
# q: sacctmgr query; a failed query stops the script instead of reading as
# "none". Always assign its output first: set -e ignores a failed $(...)
# inside a test, a for list or a pipeline.
q() { sacctmgr -n -P "$@" || { echo "sacctmgr $* failed" >&2; exit 1; }; }
case $action in
install)
  for a; do
    d=$(q show account "$a" format=Description)
    d=$(echo "$d" | tr '[:upper:]' '[:lower:]')
    if [ -z "$d" ]; then
      sacctmgr -i add account "$a" description="$desc"
    elif [ "$d" != "$desc" ]; then
      echo "account $a exists and is not Chaperone's" >&2
      exit 1
    fi
    assoc=$(q show assoc where account="$a" user="$svc" format=User)
    [ -n "$assoc" ] || sacctmgr -i add user "$svc" account="$a"
    coords=$(q show account "$a" withcoord format=Coordinators)
    echo "$coords" | tr , '\n' | grep -qx "$svc" || sacctmgr -i add coordinator account="$a" names="$svc"
  done
  ;;
uninstall)
  accounts=""
  for a; do
    d=$(q show account "$a" format=Description)
    d=$(echo "$d" | tr '[:upper:]' '[:lower:]')
    if [ "$d" = "$desc" ]; then accounts="$accounts $a"; fi
  done
  [ -n "$accounts" ] || exit 0
  for a in $accounts; do scancel -A "$a"; done
  busy=x
  for _ in $(seq 60); do
    busy=""
    for a in $accounts; do
      jobs=$(squeue -h -A "$a")
      busy="$busy$jobs"
    done
    [ -z "$busy" ] && break
    sleep 2
  done
  if [ -n "$busy" ]; then
    echo "persona jobs still running after cancel:" >&2
    echo "$busy" >&2
    exit 1
  fi
  list=$(echo $accounts | tr ' ' ,)
  ours=$(echo $accounts | tr ' ' '\n')
  users=$(q show assoc where account="$list" format=User)
  for u in $(echo "$users" | sort -u); do
    mine=$(q show assoc where user="$u" format=Account)
    others=$(echo "$mine" | sort -u | grep -vxF -e "$ours" || true)
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
