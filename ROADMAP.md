# Delivery roadmap

## Completed and integrated

- [x] Create public source repository and isolated implementation contracts.
- [x] Implement encrypted SQLite storage and consistent backup restoration tests.
- [x] Implement single-owner authentication and origin/session tests.
- [x] Preserve and integrate the SSH connection and terminal work.
- [x] Preserve and integrate the container engine and Compose recovery work.
- [x] Preserve and integrate durable scheduling work.
- [x] Preserve and integrate browser notification and Compose control updates.
- [x] Build the frontend production assets with Vite.

## Verification still required

- [ ] Run and record the complete Go test suite on a quiet checkout.
- [ ] Run and record the Go manager build on the integrated commit.
- [ ] Complete and verify SSH host, jump, terminal, file, and tunnel workflows.
- [ ] Complete and verify container, Compose, and resource operations.
- [ ] Complete and verify automatic schedules and interrupted-run recovery.
- [ ] Wire and verify all operational browser controls.
- [ ] Build both Linux architectures and exercise disposable workloads.
- [ ] Verify production browser layouts and accessibility.
- [ ] Deploy on a verified private LAN host with owner setup.
- [ ] Complete per-surface feature inventory and evidence.
- [ ] Publish a versioned downloadable release and recovery documentation.
