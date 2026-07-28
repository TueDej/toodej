# Agent Directives & Technical Guidelines

This document contains mandatory rules for AI coding agents (such as OpenCode) working on this repository.

## 1. Go Version & Compatibility Limits
* **Strict Go Baseline:** The project MUST remain strictly compatible with **Go 1.24** (`go 1.24.4` / `go 1.24`).
* **Toolchain Switching:** DO NOT bump the `go` directive in `go.mod` above `1.24`.
* **Dependency Locking:**
  * When adding or upgrading Go packages, ensure their minimum Go version requirement is $\le$ 1.24.
  * Specifically, `modernc.org/sqlite` MUST be locked to a version compatible with Go 1.24 (e.g., `v1.36.x` series). Do NOT update to `v1.54.0+` or any version that mandates Go 1.25+.
* Always run `GOTOOLCHAIN=local go mod tidy` and verify builds pass on Go 1.24 before committing changes.

## 2. Deployment & Execution
* **Deployment Scripts (`deploy.sh`):**
  * MUST execute initially under standard unprivileged user privileges (`./deploy.sh`).
  * `go build` and `go mod` commands must NEVER require `sudo` to prevent permission/cache pollution issues.
  * `sudo` should only be invoked for writing system files (moving binaries to `/usr/local/bin`, configuring systemd service files, restarting systemctl services).

## 3. Tech Stack Architecture
* **Backend:** Standard Go (`net/http` or `github.com/go-chi/chi/v5`) with `modernc.org/sqlite` (pure Go SQLite).
* **Frontend:** Go HTML Templates + HTMX + Tailwind CSS CDN. Keep frontend dependencies zero-npm/zero-build step.
* **Database Location:** Production database path should default to `/var/lib/farmstore/farmstore.db`.
