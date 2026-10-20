#!/bin/sh
# Chaperone's Slurm objects. Run as a Slurm administrator (root on the
# slurmdbd/controller host):
#
#   slurm.sh install   SERVICE_USER ACCOUNT...
#   slurm.sh uninstall SERVICE_USER ACCOUNT...
#
# install: each persona account (no limits: add your own) with SERVICE_USER,
# the broker's Unix user, as its coordinator. Idempotent.
# uninstall: cancels the accounts' jobs, deletes their associations (personas
# and SERVICE_USER) and the accounts. Job history stays in accounting.
set -eu
action=${1:?} svc=${2:?}
shift 2
has() { [ -n "$(sacctmgr -n -P "$@")" ]; }
for a; do
  case $action in
  install)
    has show account "$a" format=Account || sacctmgr -i add account "$a" description="Chaperone personas"
    has show assoc where account="$a" user="$svc" format=User || sacctmgr -i add user "$svc" account="$a"
    sacctmgr -n -P show account "$a" withcoord format=Coordinators | tr , '\n' | grep -qx "$svc" ||
      sacctmgr -i add coordinator account="$a" names="$svc"
    ;;
  uninstall)
    has show account "$a" format=Account || continue
    scancel -A "$a" || true
    for _ in $(seq 60); do [ -z "$(squeue -h -A "$a")" ] && break; sleep 2; done
    sacctmgr -i delete coordinator account="$a" names="$svc" || true
    for u in $(sacctmgr -n -P show assoc where account="$a" format=User | sort -u); do
      sacctmgr -i delete user name="$u" account="$a"
    done
    sacctmgr -i delete account "$a"
    ;;
  *) echo "usage: slurm.sh install|uninstall SERVICE_USER ACCOUNT..." >&2; exit 2 ;;
  esac
done
