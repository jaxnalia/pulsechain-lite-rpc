---
name: upgrade-clients
description: >-
  Check for and apply updates to the PulseChain Docker Compose execution
  and consensus clients (go-pulse, prysm-pulse) from GitLab releases.
  Use when the user asks to upgrade/update the node, bump client versions,
  or pull the latest go-pulse / prysm-pulse images.
---

# Upgrade PulseChain Docker clients

Run from the project directory that contains `docker-compose.yml` and `.env`.
Do **not** upgrade until the user explicitly approves after the version report.
Abort immediately if release notes require a re-sync or indicate database incompatibility.

Pinned versions live in `.env`:

- `GO_PULSE_VERSION` → `registry.gitlab.com/pulsechaincom/go-pulse`
- `PRYSM_PULSE_VERSION` → `registry.gitlab.com/pulsechaincom/prysm-pulse/beacon-chain`

## 1. Check currently running versions

```shell
docker compose ps
```

Also read `.env` and confirm the image tags shown by Compose match:

- `GO_PULSE_VERSION`
- `PRYSM_PULSE_VERSION`

If containers are not running, treat `.env` as the current pinned versions and say so. If running image tags and `.env` disagree, report the mismatch and ask the user how to proceed before upgrading.

## 2. Check latest releases

Fetch the latest release tags (and notes) from GitLab:

- Go-Pulse releases: https://gitlab.com/pulsechaincom/go-pulse/-/releases
- Prysm-Pulse releases: https://gitlab.com/pulsechaincom/prysm-pulse/-/releases

Preferred machine-readable fetch:

```shell
curl -sL "https://gitlab.com/api/v4/projects/pulsechaincom%2Fgo-pulse/releases?per_page=20"
curl -sL "https://gitlab.com/api/v4/projects/pulsechaincom%2Fprysm-pulse/releases?per_page=20"
```

Use each project's newest stable `tag_name` as the candidate latest version (for example `v3.3.0`). Prefer formal releases over pre-releases (alpha, beta, rc) unless the user asks for those.

## 3. Review release notes for breaking changes

If either latest tag differs from the currently running / pinned version, carefully review **all release notes between the current version and the latest** (inclusive of intermediate releases).

Abort and report to the user (do not upgrade) if notes indicate any of:

- Required re-sync / resync / full sync from scratch
- Database incompatibility, forced migration that wipes state, or “incompatible DB”
- Other breaking changes that make an in-place upgrade unsafe without operator action beyond flag updates

When aborting, summarize the relevant release-note findings and leave `.env` / running containers unchanged.

## 4. Report versions and ask for approval

If there are no breaking changes and no database incompatibilities:

1. Report **current running / pinned versions** for go-pulse and prysm-pulse
2. Report **latest available versions** for both
3. Briefly note any non-blocking changes worth knowing (new flags, deprecations, recommended settings)
4. **Prompt the user for approval** before changing anything

If both clients are already on the latest release, report that and stop.

Do **not** pull images, edit files, or restart the node until the user approves.

## 5. Perform the upgrade (only after approval)

### A) Pull latest images

```shell
docker pull "registry.gitlab.com/pulsechaincom/go-pulse:<latest-go-pulse-tag>"
docker pull "registry.gitlab.com/pulsechaincom/prysm-pulse/beacon-chain:<latest-prysm-pulse-tag>"
```

Replace the tag placeholders with the approved latest versions.

### B) Gracefully stop the node

```shell
docker compose down
```

Compose is configured with a multi-minute stop grace period; allow shutdown to finish.

### C) Update `.env`

Set:

```env
GO_PULSE_VERSION=<latest-go-pulse-tag>
PRYSM_PULSE_VERSION=<latest-prysm-pulse-tag>
```

Keep any existing comments; only change the version values that need updating.

### D) Update `docker-compose.yml` flags if needed

From the reviewed release notes between the old and new versions, add, remove, or fix any changed client flags/options in `docker-compose.yml`. Do not change unrelated settings.

### E) Start the updated node

```shell
docker compose up -d
```

### F) Report upgrade status

Confirm to the user:

1. New `.env` versions
2. Whether `docker compose ps` shows both services up on the new image tags
3. Any compose/flag changes made
4. That they should monitor sync/health afterward (for example with the sync-status skill)

If any step fails, stop, leave the system in a safe state if possible, and report what failed and what was already changed.
