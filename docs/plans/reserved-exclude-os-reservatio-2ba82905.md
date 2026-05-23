# Reserved Exclude OS Reservation

## Overview

Today `--kube-reserved` and `--system-reserved` are subtracted from node **capacity** to derive **allocatable**. However, the "capacity" kubelet reports is **not raw hardware capacity** — it is `MemTotal` from `/proc/meminfo` (and `NumCores` from `/proc/cpuinfo`), which the kernel computes after subtracting its own reservations (kernel image, vmemmap, crashkernel, initrd, ACPI, hugepages set at boot, etc.). When the OS is patched and the new kernel reserves a different amount, **capacity** drifts and therefore **allocatable** drifts even though the operator's `kube-reserved`/`system-reserved` numbers are unchanged.

This plan investigates approaches to stabilize allocatable across kernel patches, settles the in-kubelet vs. out-of-kubelet question, and — if the universal in-kubelet approach is not viable — designs a small helper program for Linux nodes (Ubuntu and COS) that detects the OS reservation and writes a corrected `systemReserved` into the kubelet config drop-in before kubelet starts.

## Context

- **Files involved** (read for investigation; modified only in later tasks):
  - `pkg/kubelet/cadvisor/util.go` — `CapacityFromMachineInfo()` (line 41) converts `MachineInfo.MemoryCapacity` and `NumCores` into the `ResourceList` used as capacity.
  - `vendor/github.com/google/cadvisor/machine/machine.go` — cAdvisor parses `MemTotal:` from `/proc/meminfo` (regex at line 47) to populate `MachineInfo.MemoryCapacity`. This is the source of fluctuation.
  - `pkg/kubelet/cm/container_manager_linux.go` (lines 236–296) — captures `capacity` once at startup from `cadvisorInterface.MachineInfo()` and stores it on `containerManagerImpl`.
  - `pkg/kubelet/cm/node_container_manager_linux.go` — `GetNodeAllocatableReservation()` (line 291), `GetNodeAllocatableAbsolute()` (line 258), `validateNodeAllocatable()` (line 316).
  - `pkg/kubelet/nodestatus/setters.go` (lines 293–336) — applies `allocatable = capacity − reservation` per resource.
  - `pkg/kubelet/apis/config/types.go` (lines 419–438) — `SystemReserved`, `KubeReserved`, `EnforceNodeAllocatable`, `ReservedSystemCPUs`.
  - `pkg/kubelet/cm/container_manager.go` (lines 205–213) — `NodeAllocatableConfig`.
- **New artifacts (likely to be created):**
  - `cmd/kubelet-os-reservation-detector/` (or similar) — small helper binary if Option C is chosen.
  - `docs/design/node-os-reservation.md` — short design write-up.
- **Related patterns:**
  - Kubelet drop-in config pattern under `/etc/default/kubelet` (Ubuntu) and `/etc/kubernetes/kubelet/kubelet-config.json` (COS) read at kubelet startup.
  - `kube-up`/`cluster/gce/gci/configure-helper.sh` is the COS bootstrap shell script; Ubuntu uses `kubeadm`/cloud-init or distro-specific systemd units.
- **Dependencies:**
  - Linux-only data sources for "true installed RAM": `/sys/firmware/dmi/tables/DMI`, `/sys/devices/system/memory/block_size_bytes` + `memoryN/state`, cloud metadata (GCE/Azure/AWS IMDS), `dmidecode` (root + `/dev/mem`), or `/proc/iomem` (root).
  - For Ubuntu cloud images: `dmidecode` is typically present; SMBIOS exposed by every major hypervisor.
  - For COS: minimal image — SMBIOS via `/sys/firmware/dmi/tables/DMI` is available without `dmidecode`.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices. Kubelet container-manager unit tests live under `pkg/kubelet/cm/*_test.go`; any helper binary lives outside the kubelet binary and gets its own table-driven unit tests + a Linux-only integration test that runs against a real `/proc`/`/sys` fixture tree.
- Complete each task fully before moving to the next.
- **CRITICAL: all tests must pass before starting next task.**
- The first three tasks are investigation and design — they should converge on a written decision document **before** any production code is written. Do not skip them.

## Implementation Steps

### Task 1: Confirm the root cause and quantify the fluctuation

Verify the hypothesis end-to-end before designing a fix: that kubelet's capacity comes from `MemTotal` (so it includes OS-reservation effects), and measure how much it actually moves across kernel versions on the target distros.

**Files:**
- Modify: none (investigation only).
- Output: `docs/design/node-os-reservation.md` (new) — short investigation notes (target ≤ 2 pages).

- [ ] Trace the capacity path from `/proc/meminfo` → cAdvisor `machine.Info` → `cadvisor.CapacityFromMachineInfo()` → `containerManagerImpl.capacity` → `Setter` in `nodestatus/setters.go`. Confirm there is no place where "hardware RAM" is consulted.
- [ ] On a representative Ubuntu 22.04 and COS image (current + one prior kernel), capture: `MemTotal` (`/proc/meminfo`), sum of `System RAM` ranges in `/proc/iomem`, SMBIOS "Installed Size" via `dmidecode -t memory` (or parsed from `/sys/firmware/dmi/tables/DMI`), `/sys/devices/system/memory/block_size_bytes` × count of `memoryN/state == online`. Record the deltas.
- [ ] Do the same comparison for CPU: confirm that `NumCores` (from `/proc/cpuinfo`) is stable across kernel patches in practice; the OS-reservation problem is almost entirely a memory problem. Document this finding.
- [ ] Write up the data sources, their reliability, and the magnitude of fluctuation observed (target: bytes / % of capacity) in `docs/design/node-os-reservation.md` under a "Findings" section.

### Task 2: Evaluate whether kubelet can do this universally

Decide whether kubelet itself can detect OS reservation portably. This is the user's primary question.

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "Options" and "Decision" sections).

- [ ] List the candidate sources for "pre-OS-reservation" memory and rate each on **portability**, **privilege required**, **accuracy**, **availability in containerized kubelet**:
  1. SMBIOS via `/sys/firmware/dmi/tables/DMI` (root, Linux-only, present on virtually all x86 + most ARM servers; absent on bare-metal SBCs and some hypervisors that don't expose DMI).
  2. `/proc/iomem` "System RAM" sum (root, Linux-only, requires `CAP_SYS_RAWIO`).
  3. `/sys/devices/system/memory/block_size_bytes` × online blocks (no root, but only reflects what the kernel decided to expose — not true hardware total).
  4. Cloud metadata services (portable per cloud but not universal; embeds vendor coupling into kubelet).
  5. Devicetree `/sys/firmware/devicetree/base/memory@*/reg` (ARM/embedded only).
- [ ] Conclude (with rationale) whether a **universal** mechanism exists that is both (a) portable across all supported kubelet platforms and (b) does not require kubelet to gain new privileges or vendor-specific code. Expected conclusion: **no universal mechanism** — SMBIOS is the closest but fails on bare-metal/embedded and requires privileged access kubelet does not currently take; cloud metadata is per-vendor.
- [ ] Document why an "automatic in-kubelet rolling" of OS reservation into `system-reserved` is risky even if detectable: it would change the meaning of operator-set `system-reserved` values, it would invalidate cgroup enforcement assumptions (`systemReservedCgroup` is sized against operator intent, not detected slop), and it would cause allocatable to silently shrink when a kernel grows — the opposite of the stability the user wants.
- [ ] In `docs/design/node-os-reservation.md`, record the decision: **prefer an out-of-kubelet detector that adjusts `system-reserved` (or a new dedicated bucket) at bootstrap time**, with a possible follow-on kubelet feature (Task 7) once experience is gathered.

### Task 3: Choose the integration shape for the out-of-kubelet detector

Before writing code, pin down where the detector runs, how it injects the value into kubelet, and whether it adjusts `system-reserved` or introduces a new explicit bucket (e.g., `os-reserved`).

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "Design" section).

- [ ] Decide on the value injection mechanism. Two viable shapes:
  - **A. Bootstrap-time:** A one-shot binary run by the kubelet systemd unit's `ExecStartPre=` writes a drop-in to `/etc/kubernetes/kubelet-config-osreserved.yaml` that sets `systemReserved.memory` to `operator_baseline + detected_os_delta`. Kubelet reads it as normal config.
  - **B. Long-running sidecar:** detects changes on each reboot/upgrade and reconciles the config; restart kubelet on change.
  - **Recommended:** **A** — kubelet config is read once at startup, so a long-running sidecar adds no value; the OS reservation only changes across reboots.
- [ ] Decide on the accounting shape:
  - **A1.** Fold the detected delta into existing `systemReserved.memory`. Simple, no API changes, but obscures the breakdown for operators.
  - **A2.** Add a separate operator concept (`osReserved`) that kubelet treats identically to `systemReserved` but reports/logs separately. Requires a kubelet config field addition.
  - **Recommended for v1:** **A1** for minimum surface area; revisit A2 as a follow-up KEP if there is demand.
- [ ] Decide what "baseline" the detector compares against:
  - The advertised hardware memory (from SMBIOS / cloud metadata) — **target**.
  - The first-observed `MemTotal` after node provisioning, persisted to `/var/lib/kubelet/os-reservation-baseline` — **fallback** when no advertised size is available. With this fallback, the detector adjusts only on **change** from baseline, never on absolute level.
- [ ] Decide on units & rounding (e.g., round detected delta up to nearest 16 MiB) and document failure modes (detector cannot determine baseline → emits a no-op drop-in and logs a warning; never block kubelet startup).
- [ ] Pin down where the binary lives in this repo. Suggested: `cmd/kubelet-os-reservation-detector/main.go` with the bulk of logic in `pkg/kubeletosreservation/`. Confirm before writing code (see Questions).

### Task 4: Implement the OS-reservation detector library

Self-contained Go library with no kubelet dependencies (so it can be embedded into other tools later). Linux-only.

**Files:**
- Create: `pkg/kubeletosreservation/detector.go`
- Create: `pkg/kubeletosreservation/detector_test.go`
- Create: `pkg/kubeletosreservation/sources_linux.go`
- Create: `pkg/kubeletosreservation/sources_linux_test.go`
- Create: `pkg/kubeletosreservation/baseline.go`
- Create: `pkg/kubeletosreservation/baseline_test.go`

- [ ] Define a `Source` interface with `Name() string` and `AdvertisedMemoryBytes(ctx) (uint64, error)`. Implementations: `smbiosSource` (parses `/sys/firmware/dmi/tables/DMI` Type 17 records for physical memory array totals), `procIomemSource` (sums `System RAM` ranges from `/proc/iomem`), `sysfsMemorySource` (counts online memory blocks).
- [ ] Implement a `Detector` that:
  1. Reads `MemTotal` from `/proc/meminfo` (path overridable for tests).
  2. Queries sources in priority order and accepts the first result that is **within 25% of `MemTotal` and ≥ `MemTotal`**, to defend against bogus readings.
  3. Returns `delta = advertised − MemTotal` clamped to `[0, MemTotal]` and rounded up to the nearest 16 MiB (constant from Task 3).
  4. Falls back to the persisted baseline file (`baseline.go`) if no source succeeds.
- [ ] Implement baseline persistence in `baseline.go`: read/write `/var/lib/kubelet/os-reservation-baseline` (path overridable). Format: a single line with `version=1 advertised_bytes=<u64> recorded_at=<RFC3339>`. Never overwrite an existing baseline silently — write only if missing.
- [ ] Unit tests covering: SMBIOS parsing (use a recorded fixture from a real Ubuntu VM), `/proc/iomem` parser (multiple "System RAM" lines, hex ranges), sysfs block-count walk, sanity-check gate (rejects advertised < MemTotal, rejects > 1.25× MemTotal), rounding, missing-source fallback to baseline file. Use `t.TempDir()` for filesystem fixtures.
- [ ] Build tag `//go:build linux` on Linux-specific files; provide a `detector_other.go` stub returning "unsupported platform" so the package compiles on non-Linux.
- [ ] Write/update tests for the package. Run `go test ./pkg/kubeletosreservation/...` and ensure it passes.

### Task 5: Implement the CLI binary

Thin wrapper around the library that emits a kubelet config drop-in.

**Files:**
- Create: `cmd/kubelet-os-reservation-detector/main.go`
- Create: `cmd/kubelet-os-reservation-detector/main_test.go`

- [ ] Flags: `--meminfo-path` (default `/proc/meminfo`), `--baseline-path` (default `/var/lib/kubelet/os-reservation-baseline`), `--output-path` (default `/etc/kubernetes/kubelet-config-osreserved.yaml`), `--base-system-reserved-memory` (the operator's intended baseline, e.g. `500Mi`), `--dry-run`, `--v` (klog verbosity).
- [ ] On run: invoke the `Detector`, compute `effective_system_reserved_memory = base + delta`, emit a minimal kubelet config snippet (`systemReserved.memory: <value>`) using the `KubeletConfiguration` v1beta1 type (import `k8s.io/kubelet/config/v1beta1`) and `sigs.k8s.io/yaml` for serialization.
- [ ] On detection failure: emit a drop-in with `systemReserved.memory: <base>` unchanged (so kubelet still starts), log a structured warning, exit 0. Never block kubelet by exiting non-zero except on argument errors.
- [ ] Add `--print-detected-delta` for use by bootstrap scripts that want to log/monitor.
- [ ] Unit tests: golden-file tests for the emitted YAML (success, detection-failure-fallback, dry-run). Use `go test ./cmd/kubelet-os-reservation-detector/...`.
- [ ] Add the binary to `Makefile`/`hack/lib/golang.sh` ALL_TARGETS list if applicable so it builds via `make` (confirm pattern by reading `hack/lib/golang.sh`).

### Task 6: Wire it into Ubuntu and COS node bootstrap

Integrate the detector into the kubelet startup path on the two target distros so allocatable is stabilized in practice.

**Files:**
- Modify: `cluster/gce/gci/configure-helper.sh` (COS bootstrap; the function that writes the kubelet systemd unit / kubelet config — locate it before editing).
- Modify: relevant Ubuntu bootstrap path (likely `cluster/gce/gci/configure.sh` for the GCE Ubuntu flavor, or document for kubeadm users in `cluster/addons/`/release notes — confirm during the task).
- Create: `build/debs/kubelet-os-reservation-detector.service` (or equivalent systemd drop-in) — a oneshot `ExecStartPre=` for kubelet.service.

- [ ] Add `ExecStartPre=/usr/local/bin/kubelet-os-reservation-detector --base-system-reserved-memory=${KUBE_RESERVED_MEMORY_BASE}` to the kubelet systemd unit on COS. Ensure the unit's `--config` flag or drop-in directory includes the file the detector writes (`/etc/kubernetes/kubelet-config-osreserved.yaml`) by listing it after the operator's base config so it overrides only the relevant fields.
- [ ] On Ubuntu (kube-up GCE flavor), do the same. For kubeadm, document the integration in `docs/design/node-os-reservation.md` and mark out-of-scope for this PR (kubeadm maintainers can adopt later).
- [ ] Add a smoke test or kube-up e2e hook that verifies, after bring-up, `node.status.allocatable.memory` is stable across a forced kernel update on the same machine type. If a full e2e is infeasible in CI, document the manual verification procedure in `docs/design/node-os-reservation.md`.
- [ ] Run shellcheck / existing bootstrap lints on the modified scripts.

### Task 7 (optional follow-up — only if Task 2 surfaced a credible universal path): in-kubelet feature

Track as a future KEP, do not implement in this PR unless Task 2 reversed the recommendation.

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "Future Work" pointing to a follow-up KEP).

- [ ] Sketch the KEP shape (feature gate name, e.g. `DetectOSReservedMemory`; new `KubeletConfiguration` field `osReservedAutoDetect bool`; behavior: kubelet uses the same detector library at startup and folds the delta into `systemReserved` internally, logging the detected value as a node event).
- [ ] Note explicit non-goals: dynamic re-detection at runtime (out of scope — capacity is read once), CPU OS-reservation handling (Task 1 should have shown it's not material), Windows nodes (out of scope — different reservation model).

## Questions

1. **Binary location in this repo.** The detector is Linux-node-specific and tightly coupled to kubelet config. Where should it live?
   - Option A: `cmd/kubelet-os-reservation-detector/` + `pkg/kubeletosreservation/` in k/k. Ships and versions with kubelet.
   - Option B: Separate repo (`kubernetes/node-os-reservation-detector` or under `kubernetes-sigs/`). Lighter review burden but cross-repo coordination cost.
   - Option C: Folded into an existing node-side tool (e.g., extend `kubeadm`'s preflight, or COS's `configure-helper.sh` inline in shell).
   - **Suggested:** **Option A** — keeps the implementation honest about kubelet config schemas (compile-time-checked against `k8s.io/kubelet/config/v1beta1`) and avoids a release-train split.

2. **Accounting shape — fold into `systemReserved` or introduce `osReserved`?** (See Task 3.)
   - Option A: Fold into `systemReserved`, no API change. Simplest.
   - Option B: New `osReserved` field in `KubeletConfiguration`, reported separately in node status.
   - **Suggested:** **Option A** for v1; revisit B in the follow-up KEP if operators ask for visibility.

3. **Baseline source priority.** SMBIOS first, or persisted "first-observed" baseline first?
   - Option A: SMBIOS → `/proc/iomem` → sysfs blocks → persisted baseline. Detects real installed RAM where possible; only falls back to persisted baseline on bare/embedded systems without SMBIOS.
   - Option B: Persisted baseline first, hardware sources only when no baseline exists. More predictable but requires a "good" first boot.
   - **Suggested:** **Option A** — matches the user's stated goal of using hardware as the reference where it's reliably available.

4. **Scope of this PR vs. follow-ups.** Should Task 7 (in-kubelet feature gate) be included or split?
   - Option A: This PR ends after Task 6 (out-of-kubelet detector landed + wired into COS/Ubuntu kube-up). KEP for in-kubelet feature follows separately.
   - Option B: Bundle everything (detector + kubelet feature gate) into one larger change.
   - **Suggested:** **Option A** — keeps the change reviewable and lets the detector bake on real nodes before kubelet itself takes a dependency on the logic.
