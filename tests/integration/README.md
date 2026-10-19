# Disposable service integration tests

The GitHub-hosted [integration workflow](../../.github/workflows/integration.yml) starts two **independent, disposable** services:

- Official Rocky 9 FreeIPA server in a rootless Podman container, with *new, throwaway* admin credentials and groups.
- A version-pinned, containerized Slurm controller, `slurmdbd`, MariaDB and one worker. `sacctmgr` is used against the real database.

The Go `integration`-tagged test executes Chaperone's **actual `commandBackend.ensure`** implementation. Thin `ipa` and `sacctmgr` command adapters invoke the real tools inside these disposable containers. It runs `sacctmgr` as a non-root Slurm coordinator of the persona accounts, as in production. It covers first-time persona creation, idempotent repeat provisioning, group revocation, profile-specific Slurm associations and account-collision refusal.

**No institutional infrastructure, secrets, VPN, self-hosted runner, or local HPC connection is used.** No CI secrets are required. FreeIPA ports are not published; the Slurm SSH test port binds loopback only. Containers/volumes are removed even after a failure.

To rerun in GitHub, open **Actions → Disposable FreeIPA and Slurm integration → Run workflow**. Go unit tests remain in `ci.yml`.

## Important boundaries

This integration job tests FreeIPA/Slurm administrative operations, *not* end-to-end Slurm job execution under SSSD-resolved persona UIDs. Containers do not share a FreeIPA-backed NSS service. It also does not validate MUNGE-to-broker authentication, Kerberos delegation least privilege, ACLs, Apptainer, profile network policies, revocation of running sessions, or account deletion. Those are separate acceptance gates.

The fixtures are intentionally heavyweight for CI (~FreeIPA systemd plus Slurm/MariaDB); for quick iteration, use `go test ./...` first. The configuration pins the disposable Slurm image to `25.11.4`, which **differs from the institutional Slurm 24.11.5**. A future matrix can add 24.11 after an image is available.

**Never point these tests at production.** The bootstrap requires GitHub Actions and the Go live test requires `CHAPERONE_DISPOSABLE_INTEGRATION=1` and `CHAPERONE_TEST_ISOLATED=yes`.
