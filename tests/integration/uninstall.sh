#!/usr/bin/env bash
# deploy/ipa.py and deploy/slurm.sh uninstall leave nothing of Chaperone behind.
set -Eeuo pipefail
: "${GITHUB_ACTIONS:?disposable CI runner only}"
ipa_admin() { podman exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache chaperone-test-ipa ipa "$@"; }
# Production order: Slurm first (jobs gone), then the directory.
docker exec -i slurmctld sh -s uninstall chaperone agents agents-special lab-foreign < deploy/slurm.sh
# Retire: personas disabled and kept with their groups; the broker's rights go.
podman exec chaperone-test-ipa python3 /tmp/ipa.py retire ipa.chaperone.test chaperone-users
podman exec chaperone-test-ipa python3 /tmp/ipa.py retire ipa.chaperone.test chaperone-users # idempotent
left=0
for p in chaperone-ci-human-ai chaperone-ci-human-ai-special; do
  ipa_admin user-show "$p" --all --raw | grep -qi 'nsaccountlock: TRUE' || { echo "retire did not disable $p"; left=1; }
  ipa_admin group-show "$p" >/dev/null || { echo "retire deleted group $p"; left=1; }
done
ipa_admin role-show 'Chaperone broker' >/dev/null 2>&1 && { echo "retire kept the broker role"; left=1; }
# Installing again re-enables them.
podman exec chaperone-test-ipa python3 /tmp/ipa.py install ipa.chaperone.test chaperone-users --members chaperone-ci-human --ban restricted admins
ipa_admin user-show chaperone-ci-human-ai --all --raw | grep -qi 'nsaccountlock: TRUE' && { echo "install did not re-enable the persona"; left=1; }
[[ $left == 0 ]] || exit 1
# Purge: everything goes.
podman exec chaperone-test-ipa python3 /tmp/ipa.py uninstall ipa.chaperone.test chaperone-users
podman exec chaperone-test-ipa python3 /tmp/ipa.py uninstall ipa.chaperone.test foreign-users
left=0
for obj in "user-show chaperone-ci-human-ai" "user-show chaperone-ci-human-ai-special" "group-show chaperone-ci-human-ai" \
  "group-show chaperone-users" "role-show Chaperone broker" "privilege-show Chaperone persona provisioning" \
  "permission-show Chaperone persona group membership" "permission-show Chaperone persona description" "service-show chaperone/ipa.chaperone.test"; do
  read -r verb name <<<"$obj"
  if ipa_admin "$verb" "$name" >/dev/null 2>&1; then echo "left behind: $obj"; left=1; fi
done
ipa_admin user-show chaperone-ci-human >/dev/null || { echo "uninstall deleted the human"; left=1; }
for a in agents agents-special; do
  [[ -z $(docker exec slurmctld sacctmgr -n -P show account "$a" format=Account) ]] || { echo "left behind: Slurm account $a"; left=1; }
done
[[ -z $(docker exec slurmctld sacctmgr -n -P show user chaperone format=User) ]] || { echo "left behind: Slurm user chaperone"; left=1; }
docker exec slurmctld sacctmgr -n -P show user root format=User | grep -qx root || { echo "uninstall touched other Slurm users"; left=1; }
[[ -n $(docker exec slurmctld sacctmgr -n -P show account lab-foreign format=Account) ]] || { echo "uninstall deleted a foreign Slurm account"; left=1; }
ipa_admin group-show foreign-users >/dev/null || { echo "uninstall deleted a foreign group"; left=1; }
exit $left
