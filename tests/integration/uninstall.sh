#!/usr/bin/env bash
# deploy/ipa.py and deploy/slurm.sh uninstall leave nothing of Chaperone behind.
set -Eeuo pipefail
: "${GITHUB_ACTIONS:?disposable CI runner only}"
ipa_admin() { podman exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache chaperone-test-ipa ipa "$@"; }
podman exec chaperone-test-ipa python3 /tmp/ipa.py uninstall ipa.chaperone.test chaperone-users
docker exec -i slurmctld sh -s uninstall chaperone agents agents-special < deploy/slurm.sh
left=0
for obj in "user-show chaperone-ci-human-ai" "user-show chaperone-ci-human-ai-special" "group-show chaperone-ci-human-ai" \
  "group-show chaperone-users" "role-show Chaperone broker" "privilege-show Chaperone persona provisioning" \
  "permission-show Chaperone persona group membership" "service-show chaperone/ipa.chaperone.test"; do
  read -r verb name <<<"$obj"
  if ipa_admin "$verb" "$name" >/dev/null 2>&1; then echo "left behind: $obj"; left=1; fi
done
ipa_admin user-show chaperone-ci-human >/dev/null || { echo "uninstall deleted the human"; left=1; }
for a in agents agents-special; do
  [[ -z $(docker exec slurmctld sacctmgr -n -P show account "$a" format=Account) ]] || { echo "left behind: Slurm account $a"; left=1; }
done
[[ -z $(docker exec slurmctld sacctmgr -n -P show user chaperone format=User) ]] || { echo "left behind: Slurm user chaperone"; left=1; }
docker exec slurmctld sacctmgr -n -P show user root format=User | grep -qx root || { echo "uninstall touched other Slurm users"; left=1; }
exit $left
