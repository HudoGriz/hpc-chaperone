#!/usr/bin/env bash
set -u
if [[ -n "${RUNNER_TEMP:-}" && -d "$RUNNER_TEMP/chaperone-slurm" ]]; then
  (cd "$RUNNER_TEMP/chaperone-slurm" && docker compose down -v --remove-orphans) || true
fi
docker rm -f chaperone-test-ipa >/dev/null 2>&1 || true
docker volume rm chaperone-test-ipa-data >/dev/null 2>&1 || true
