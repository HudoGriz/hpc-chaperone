#!/usr/bin/env bash
# Start entirely disposable FreeIPA and Slurm for GitHub-hosted integration CI.
# Never point this test at production: all API clients use docker exec only.
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
if docker container inspect "$IPA_CONTAINER" >/dev/null 2>&1 || docker container inspect slurmctld >/dev/null 2>&1; then
  echo 'CI test container name already in use; refusing to reuse it' >&2
  exit 1
fi

umask 077
password="Ci-A1!$(openssl rand -hex 18)"
echo "::add-mask::$password"
printf '%s\n' "$password" > "$PASS_FILE"
unset password

echo 'Starting throwaway official FreeIPA container (no published ports)'
docker volume create "$IPA_VOLUME" >/dev/null
docker run -d --name "$IPA_CONTAINER" --hostname ipa.chaperone.test \
  --read-only --tmpfs /run --tmpfs /tmp \
  -v "$IPA_VOLUME:/data" \
  -e "PASSWORD=$(cat "$PASS_FILE")" \
  "$IPA_IMAGE" ipa-server-install -U -r CHAPERONE.TEST --no-ntp --skip-mem-check >/dev/null

freeipa_ready=false
for _ in $(seq 1 90); do
  if docker exec "$IPA_CONTAINER" test -s /etc/ipa/default.conf 2>/dev/null \
    && docker exec -i -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" kinit admin < "$PASS_FILE" >/dev/null 2>&1 \
    && docker exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" ipa ping >/dev/null 2>&1; then
    freeipa_ready=true
    break
  fi
  sleep 5
done
if [[ "$freeipa_ready" != true ]]; then
  echo 'FreeIPA did not initialize within 7.5 minutes' >&2
  docker logs "$IPA_CONTAINER" --tail 140 || true
  exit 1
fi
# Password needed no longer. Kerberos credentials stay in disposable container.
rm -f "$PASS_FILE"
echo 'FreeIPA ready, creating disposable test groups'
for group in agents research genome-project restricted; do
  docker exec -e KRB5CCNAME=FILE:/tmp/chaperone-ci-admin.ccache "$IPA_CONTAINER" \
    ipa group-add "$group" --desc="Chaperone disposable CI group: $group"
done

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
for account in agents agents-special; do
  docker exec slurmctld sacctmgr -i add account name="$account" description="Chaperone CI $account"
done

echo 'Services ready: real FreeIPA CLI and real sacctmgr, disposable runner only.'
