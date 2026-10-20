# Disposable service integration tests

The GitHub-hosted [integration workflow](../../.github/workflows/integration.yml) starts two **independent, disposable** services:

- Official Rocky 9 FreeIPA server in a rootless Podman container, with *new, throwaway* admin credentials and groups.
- A version-pinned, containerized Slurm controller, `slurmdbd`, MariaDB and one worker. `sacctmgr` is used against the real database.

`bootstrap.sh` installs Chaperone's objects with the production scripts (`deploy/ipa.py`, `deploy/slurm.sh`, each run twice for idempotence, using externally managed eligibility membership (and refusal of foreign Slurm accounts)) and fetches the broker keytab with the host principal. The Go `integration`-tagged test then runs Chaperone's **actual `commandBackend.ensure`** with the broker's real rights: `ipa` as the broker service principal, `sacctmgr` as the Slurm coordinator. It covers persona and persona-group creation, idempotence, the broker's inability to touch other groups, drift and association refusals, and collisions. `uninstall.sh` then runs both uninstalls and fails on anything left behind.

**No institutional infrastructure, secrets, VPN, self-hosted runner, or local HPC connection is used.** No CI secrets are required. FreeIPA ports are not published; the Slurm SSH test port binds loopback only. Containers/volumes are removed even after a failure.

To rerun in GitHub, open **Actions → Disposable FreeIPA and Slurm integration → Run workflow**. Go unit tests remain in `ci.yml`.

## Important boundaries

This job tests FreeIPA/Slurm administration, *not* Slurm jobs under SSSD-resolved persona UIDs (containers share no FreeIPA-backed NSS). The playbooks, sudo and ACLs are covered by `tests/e2e`; job execution on real nodes is a site pilot step.

The fixtures are intentionally heavyweight for CI (~FreeIPA systemd plus Slurm/MariaDB); for quick iteration, use `go test ./...` first. The disposable Slurm image is pinned to `25.11.4`; sites on older Slurm should pilot first.

**Never point these tests at production.** The bootstrap requires GitHub Actions and the Go live test requires `CHAPERONE_DISPOSABLE_INTEGRATION=1` and `CHAPERONE_TEST_ISOLATED=yes`.
