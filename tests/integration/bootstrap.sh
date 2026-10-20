#!/usr/bin/env bash
# Start entirely disposable FreeIPA and Slurm for GitHub-hosted integration CI.
# Never point this test at production: all API clients use local container exec only.
set -Eeuo pipefail

: "${RUNNER_TEMP:?Run only on a disposable CI runner with RUNNER_TEMP set}"
: "${GITHUB_ACTIONS:?This bootstrap deliberately requires GitHub Actions}"

IPA_CONTAINER=chaperone-test-ipa
IPA_IMAGE=freeipa/freeipa-server:rocky-9-4.13.4
IPA_VOLUME=chaperone-test-ipa-data
SLURM_IMAGE=giovtorres/slurm-docker-cluster:25.11.4
SLURM_GIT_REV=9001503b42e7277c7ff45c08aeecdb6427474d68
SLURM_DIR="$RUNNER_TEMP/chaperone-slurm"
PASS_FILE="$RUNNER_TEMP/chaperone-ipa-pass"

# Fail if someone left a foreign container under the names used by the tests.
if podman container inspect "$IPA_CONTAINER" >/dev/null 2>&1 || docker container inspect slurmctld >/dev/null 2>&1; then
  echo 'CI test container name already in use; refusing to reuse it' >&2
  exit 1
fi

umask 077
password="Ci-A1!$(openssl rand -hex 18)"
echo "::add-mask::$password"
printf '%s\n' "$password" > "$PASS_FILE"
unset password

# GitHub's Ubuntu images sometimes select an outdated /usr/bin/crun for Podman.
# Explicitly use the newer runtime when installed to support rootless systemd.
if [[ -x /usr/local/bin/crun ]]; then
  mkdir -p "$HOME/.config/containers"
  printf '[engine]\nruntime = "/usr/local/bin/crun"\n' > "$HOME/.config/containers/containers.conf"
fi
podman info --format '{{.Host.OCIRuntime.Path}}'
echo 'Starting throwaway official FreeIPA container with rootless Podman (no published ports)'
podman volume create "$IPA_VOLUME" >/dev/null
podman run -d --name "$IPA_CONTAINER" --hostname ipa.chaperone.test \
  --read-only --tmpfs /run --tmpfs /tmp \
  -v "$IPA_VOLUME:/data" \
  -e "PASSWORD=$(cat "$PASS_FILE")" \
  "$IPA_IMAGE" ipa-server-install -U -r CHAPERONE.TEST --no-ntp --skip-mem-check >/dev/null

freeipa_ready=false
for _ in $(seq 1 90); do
  if podman exec "$IPA_CONTAINER" test -s /etc/ipa/default.conf 2>/dev/null \
    && podman exec -i -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" kinit admin < "$PASS_FILE" >/dev/null 2>&1 \
    && podman exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" ipa ping >/dev/null 2>&1; then
    freeipa_ready=true
    break
  fi
  if [[ "$(podman inspect -f '{{.State.Running}}' "$IPA_CONTAINER" 2>/dev/null)" != true ]]; then
    echo 'FreeIPA container exited before readiness (see logs below)' >&2
    podman logs "$IPA_CONTAINER" --tail 140 || true
    exit 1
  fi
  sleep 5
done
if [[ "$freeipa_ready" != true ]]; then
  echo 'FreeIPA did not initialize within 7.5 minutes' >&2
  podman logs "$IPA_CONTAINER" --tail 140 || true
  exit 1
fi
# Password needed no longer. Kerberos credentials stay in disposable container.
rm -f "$PASS_FILE"
echo 'FreeIPA ready: test human, a restricted group, then deploy/ipa.py as in production'
ipa_admin() { podman exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" ipa "$@"; }
ipa_admin user-add chaperone-ci-human --first=CI --last=Human
ipa_admin group-add restricted --desc='Chaperone disposable CI group'
podman exec -i "$IPA_CONTAINER" sh -c 'cat > /tmp/ipa.py' < deploy/ipa.py
podman exec "$IPA_CONTAINER" python3 /tmp/ipa.py install ipa.chaperone.test chaperone-users --members chaperone-ci-human admin
# Authoritative members: admin leaves the group; a second run changes nothing.
podman exec "$IPA_CONTAINER" python3 /tmp/ipa.py install ipa.chaperone.test chaperone-users --members chaperone-ci-human
podman exec "$IPA_CONTAINER" python3 /tmp/ipa.py install ipa.chaperone.test chaperone-users --members chaperone-ci-human | grep -q '^member ' && { echo 'members not idempotent' >&2; exit 1; }
ipa_admin group-show chaperone-users --raw | grep -q 'uid=admin,' && { echo 'admin not removed from the eligible group' >&2; exit 1; }
podman exec "$IPA_CONTAINER" python3 /tmp/ipa.py install ipa.chaperone.test chaperone-users # no --members: left alone
# A same-named group Chaperone did not make is refused, never adopted.
ipa_admin group-add foreign-users --desc='Somebody else'
if podman exec "$IPA_CONTAINER" python3 /tmp/ipa.py install ipa.chaperone.test foreign-users --members admin; then
  echo 'foreign eligibility group adopted' >&2; exit 1
fi
# The broker's keytab, fetched with the broker host's own principal (as on a site).
podman exec -e KRB5CCNAME=FILE:/tmp/host.ccache "$IPA_CONTAINER" sh -c \
  'kinit -k host/ipa.chaperone.test && ipa-getkeytab -p chaperone/ipa.chaperone.test -k /tmp/broker.keytab && kdestroy'
podman exec "$IPA_CONTAINER" kinit -kt /tmp/broker.keytab -c FILE:/tmp/broker.ccache chaperone/ipa.chaperone.test

echo 'Starting disposable Slurm accounting/controller/one worker'
mkdir -p "$SLURM_DIR"
git -C "$SLURM_DIR" init -q
git -C "$SLURM_DIR" remote add origin https://github.com/giovtorres/slurm-docker-cluster.git
git -C "$SLURM_DIR" fetch -q --depth 1 origin "$SLURM_GIT_REV"
git -C "$SLURM_DIR" checkout -q --detach FETCH_HEAD
# Match the image's Slurm version; do not build Slurm from source in CI.
printf 'SLURM_VERSION=25.11.4\nCPU_WORKER_COUNT=1\nSSH_PORT=127.0.0.1:3022\n' > "$SLURM_DIR/.env"
docker pull "$SLURM_IMAGE"
docker tag "$SLURM_IMAGE" slurm-docker-cluster:25.11.4
(
  cd "$SLURM_DIR"
  docker compose up -d --no-build mysql slurmdbd slurmctld cpu-worker
)
slurm_ready=false
for _ in $(seq 1 90); do
  if docker exec slurmctld sacctmgr -n -P show cluster 2>/dev/null | grep -q . \
    && docker exec slurmctld scontrol ping 2>/dev/null | grep -qi 'UP'; then
    slurm_ready=true
    break
  fi
  sleep 5
done
if [[ "$slurm_ready" != true ]]; then
  echo 'Slurm accounting/controller did not initialize' >&2
  (cd "$SLURM_DIR" && docker compose ps && docker compose logs --tail 100 slurmctld slurmdbd mysql) || true
  exit 1
fi
# Broker service user, same UID on controller and slurmdbd; deploy/slurm.sh
# makes it coordinator of the persona accounts, as in production.
docker exec slurmctld useradd -r -M -s /sbin/nologin chaperone
docker exec slurmdbd useradd -r -M -s /sbin/nologin -u "$(docker exec slurmctld id -u chaperone)" chaperone
docker exec -i slurmctld sh -s install chaperone agents - agents-special 'MaxJobs=7 GrpTRES=cpu=4' < deploy/slurm.sh
docker exec -i slurmctld sh -s install chaperone agents - agents-special 'MaxJobs=7 GrpTRES=cpu=4' < deploy/slurm.sh # idempotent
# Limits land on the account association only where a profile set them.
docker exec slurmctld sacctmgr -n -P show assoc where account=agents-special format=User,MaxJobs,GrpTRES | grep -qx '|7|cpu=4' || { echo 'agents-special limits not applied' >&2; exit 1; }
docker exec slurmctld sacctmgr -n -P show assoc where account=agents format=User,MaxJobs | grep -qx '|' || { echo 'agents got limits it was not given' >&2; exit 1; }
# An existing account Chaperone did not make is refused, and uninstall leaves it.
docker exec slurmctld sacctmgr -i add account lab-foreign description=lab
if docker exec -i slurmctld sh -s install chaperone lab-foreign - < deploy/slurm.sh; then
  echo 'foreign Slurm account adopted' >&2; exit 1
fi

echo 'Services ready: real FreeIPA CLI and real sacctmgr, disposable runner only.'
