# Reserved Exclude OS Reservation

## Overview

Today `--kube-reserved` and `--system-reserved` are subtracted from node **capacity** to derive **allocatable**. However, the "capacity" kubelet reports is **not raw hardware capacity** — it is `MemTotal` from `/proc/meminfo`, which the kernel computes after subtracting its own reservations (kernel image, `vmemmap`, `crashkernel`, `initrd`, ACPI tables, firmware-reserved regions, hugepages set at boot, MMIO holes, etc.). When the OS is patched and the new kernel reserves a different amount, **capacity** drifts and therefore **allocatable** drifts even though the operator's `kube-reserved`/`system-reserved` numbers are unchanged.

This plan (a) does a deeper investigation and writes the full sources-and-portability matrix into a design document, (b) determines whether any source is portable enough to drive an in-kubelet feature (gated by an opt-in feature gate + KubeletConfiguration field for backward compatibility), and (c) plans only the *reading mechanism* of an external Linux-only helper for Ubuntu and COS — bootstrap-script wiring is **out of scope** per the user's instructions.

## Context

- **Capacity path (read-only, traced for investigation):**
  - `vendor/github.com/google/cadvisor/machine/machine.go:175` — `GetMachineMemoryCapacity()` reads `/proc/meminfo` and matches `memoryCapacityRegexp` (`MemTotal:\s*([0-9]+) kB`, line 47). This is the *single* source of memory capacity.
  - `vendor/github.com/google/cadvisor/machine/info.go:73` — calls `GetMachineMemoryCapacity()`, then `info.go:137` stores it in `MachineInfo.MemoryCapacity`.
  - `pkg/kubelet/cadvisor/util.go:41` — `CapacityFromMachineInfo()` converts `MemoryCapacity` and `NumCores` directly into the `v1.ResourceList` exposed as capacity.
  - `pkg/kubelet/cm/container_manager_linux.go:240-244` — calls `cadvisorInterface.MachineInfo()` once at startup, calls `CapacityFromMachineInfo`, stores the result on `containerManagerImpl.capacity`. **Never re-read.**
  - `pkg/kubelet/cm/node_container_manager_linux.go:258-281` — `GetNodeAllocatableAbsolute()` subtracts `SystemReserved` + `KubeReserved` from that cached `capacity`.
  - `pkg/kubelet/cm/node_container_manager_linux.go:291-312` — `GetNodeAllocatableReservation()` returns the reservation map (no hardware lookup).
  - `pkg/kubelet/cm/node_container_manager_linux.go:316-334` — `validateNodeAllocatable()` rejects configs where reservation > capacity.
  - `pkg/kubelet/nodestatus/setters.go:293-336` — final `allocatable = capacity − reservation` applied per resource onto `node.Status`.
- **Config surface (modified only if in-kubelet path is chosen):**
  - `pkg/kubelet/apis/config/types.go:419-438` — `SystemReserved`, `KubeReserved`, `EnforceNodeAllocatable`, `ReservedSystemCPUs` live here on `KubeletConfiguration`.
  - `staging/src/k8s.io/kubelet/config/v1beta1/types.go` — the externally-versioned `KubeletConfiguration` that real config files bind to; any new field must land here too.
  - `pkg/features/kube_features.go` — feature-gate registry; new gate registered here.
- **New artifacts likely to be created (this PR):**
  - `docs/design/node-os-reservation.md` — the full investigation, sources/portability matrix, decision, and feature-gate design.
- **Out of scope per user feedback:** bootstrap script changes (`cluster/gce/gci/configure-helper.sh` etc.), packaging the external detector as a systemd unit, kube-up/kubeadm wiring, e2e plumbing for the external helper.
- **Related patterns:** Existing kubelet feature gates that gate node-capacity-affecting behavior (e.g. `NodeSwap` and `MutableCSINodeAllocatableCount` in `pkg/features/kube_features.go`) — model the new gate after these.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices. Kubelet container-manager unit tests live under `pkg/kubelet/cm/*_test.go`; any feature-gate-related logic gets table-driven unit tests with the gate both on and off (mirroring the `NodeSwap` pattern in `pkg/kubelet/nodestatus/setters_test.go`).
- Complete each task fully before moving to the next.
- **CRITICAL: all tests must pass before starting the next task.**
- Tasks 1–3 are investigation and design — they converge on `docs/design/node-os-reservation.md` **before** any production code is written. Do not skip them.
- Per user feedback: the external detector is planned only at the "how does the program read the data" level. The in-kubelet path is the primary design effort.

## Implementation Steps

### Task 1: Confirm the root cause and quantify the fluctuation

Verify the hypothesis end-to-end before designing a fix: that kubelet's capacity comes from `MemTotal` (so it includes OS-reservation effects), and measure how much it actually moves across kernel versions on the target distros.

**Files:**
- Modify: none (investigation only).
- Output: `docs/design/node-os-reservation.md` (new) — start the doc with a "Findings" section.

- [ ] Trace the capacity path end-to-end and record it in the design doc: `/proc/meminfo` → `vendor/github.com/google/cadvisor/machine/machine.go:175` → `vendor/github.com/google/cadvisor/machine/info.go:73,137` → `pkg/kubelet/cadvisor/util.go:41` (`CapacityFromMachineInfo`) → `pkg/kubelet/cm/container_manager_linux.go:240-244` (`containerManagerImpl.capacity`, captured **once at startup**) → `pkg/kubelet/cm/node_container_manager_linux.go:258` (`GetNodeAllocatableAbsolute`) → `pkg/kubelet/nodestatus/setters.go:293-316`. Confirm there is no place where "hardware RAM" is consulted.
- [ ] Confirm that capacity is **never re-read** during the kubelet lifetime — fluctuation only manifests across kubelet restarts (which is what happens on kernel-package updates).
- [ ] On a representative Ubuntu 22.04 and COS image (current + one prior kernel), capture the following on the *same* VM size and record deltas:
  - `MemTotal` from `/proc/meminfo`
  - Sum of `System RAM` ranges in `/proc/iomem`
  - SMBIOS Type 17 (`Memory Device`) "Size" entries via `dmidecode -t memory` (and via raw parse of `/sys/firmware/dmi/tables/DMI` so we know the no-`dmidecode` path works)
  - `/sys/devices/system/memory/block_size_bytes` × count of `memoryN/state == online`
  - `MemTotal` advertised by cloud metadata (GCE `instance/machine-type` resolves to a static GiB number)
  - Kernel cmdline reservations (`crashkernel=`, `hugepages=`, `mem=`)
- [ ] Do the same comparison for CPU: confirm that `NumCores` (from `/proc/cpuinfo`) is stable across kernel patches in practice; the OS-reservation problem is almost entirely a memory problem. Record this finding so the rest of the plan can scope to memory only.
- [ ] Write up data sources, observed magnitude (bytes / % of capacity), and which kernel-version transitions caused the largest jumps. Target ≤ 2 pages for this section.

### Task 2: Build a full sources & portability matrix

Enumerate **every** source of "pre-OS-reservation memory size" that kubelet (or a helper) could consult, and rate each on portability, privilege, accuracy, and applicability. This task expands the deeper investigation the user asked for.

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "Sources" section with a portability matrix table).

- [ ] Document each source below with the same template: **what it is**, **where the data lives**, **privileges required**, **OS coverage** (Linux distros, Windows, BSD), **arch coverage** (x86_64, arm64, riscv64, ppc64le, s390x), **virtualization coverage** (bare metal, KVM, Hyper-V, VMware, Xen, Firecracker, gVisor, kata), **container-friendliness** (works when kubelet runs in a container with stock mounts?), **stability across kernel upgrades**, **accuracy** (is this *physical RAM* or only "what the kernel decided to expose"?), and **known failure modes**.

  1. **SMBIOS Type 17 via `/sys/firmware/dmi/tables/DMI`** — parse raw SMBIOS binary; sum `Memory Device` sizes (ignoring "Not Installed" slots). No `dmidecode` dependency needed (file is world-readable on most distros; root on some). Linux-only path. Present on virtually all x86 hypervisors and bare metal; **missing** on many arm64 cloud VMs (especially AWS Graviton, GCE T2A), most arm64 SBCs, Firecracker, and gVisor.
  2. **SMBIOS via `dmidecode -t memory`** — same data as #1 but requires `dmidecode` binary and `/dev/mem` or `/sys/firmware/dmi/tables/DMI` access. Convenient but adds a runtime dependency; COS does not ship `dmidecode`. Identical coverage caveats as #1.
  3. **`/proc/iomem` "System RAM" sum** — requires `CAP_SYS_RAWIO` (or `kernel.kptr_restrict=0`); otherwise addresses are zeroed and parsing fails. Sums all kernel-known RAM ranges **including** ranges reserved for `Kernel code`, `Reserved`, `crashkernel`, etc. Linux-only. Works on every arch and every hypervisor that boots Linux. **Subtle:** still excludes physical memory the *firmware* hid before the kernel saw it (firmware-reserved/MMIO holes), so it's closer to `MemTotal + kernel reservations` than to "true physical RAM."
  4. **`/sys/devices/system/memory/block_size_bytes` × online blocks** — no root required. Reflects *what the kernel exposes*, not what the firmware exposes; therefore drifts in lockstep with kernel reservations and is **useless as a baseline**. Document this explicitly so future readers don't propose it.
  5. **`/proc/cmdline` + parse `crashkernel=`/`hugepages=`/`mem=`** — no root. Captures *some* explicit boot-time reservations but misses kernel-image size, `vmemmap`, ACPI, etc. Only useful as a tie-breaker, not as a primary source.
  6. **Cloud metadata services**:
     - GCE: `http://metadata.google.internal/computeMetadata/v1/instance/machine-type` → look up static memory size from machine-type catalog (requires the kubelet to ship the catalog or call a second API).
     - AWS IMDSv2: `instance-type` → DescribeInstanceTypes (requires AWS API call + IAM).
     - Azure IMDS: `instance/compute/vmSize` → similar catalog lookup.
     - Each is portable *within* a cloud but pulls vendor-specific code into kubelet; **non-starter for in-kubelet** because the kubelet does not currently link any cloud SDK and the in-tree cloud providers are being removed.
  7. **Devicetree `/sys/firmware/devicetree/base/memory@*/reg`** — ARM/embedded only; covers the (few) arm64 hosts that have no SMBIOS but do have DT. Read by parsing big-endian cells from a binary blob. No root required.
  8. **EFI memory map (`/sys/firmware/efi/memmap`)** — UEFI-only; sums regions of type `ConventionalMemory` + `LoaderCode/Data` + `BootServicesCode/Data`. Linux-only, root usually required. Good fidelity to true physical RAM where present but not available on non-EFI boots.
  9. **`/proc/zoneinfo` `present` pages** — same data class as `/sys/devices/system/memory` (kernel-known); not a baseline.
  10. **edac DIMM totals (`/sys/devices/system/edac/mc/`)** — cAdvisor already uses this for `GetMachineMemoryByType` (`vendor/github.com/google/cadvisor/machine/machine.go:194`). Only populated by edac drivers; absent on cloud VMs and many ARM systems. Not a reliable baseline.
  11. **Operator-provided baseline** — explicit `KubeletConfiguration` field declaring "this node has X bytes of physical RAM." Zero portability problem (operator knows their hardware) but defeats the user's goal of automation.
  12. **Persisted first-observed baseline** — on first kubelet start after install, record the SMBIOS/iomem result (or `MemTotal` if nothing else works) to `/var/lib/kubelet/os-reservation-baseline`; on subsequent starts, *only adjust based on delta from this stored value*. Zero portability problem; fails closed (no adjustment) on the very first boot — but that first boot is what the operator originally sized for, so this is acceptable.
- [ ] Render the matrix as a markdown table in the design doc with one column per source and rows for the rating dimensions above.
- [ ] Conclude (with rationale): is there a **universal** mechanism? Expected outcome — no source works for every supported kubelet platform; SMBIOS is the most portable on Linux x86 but fails on ARM cloud VMs and Firecracker; cloud metadata is per-vendor; the only fully-portable approach is the persisted-baseline fallback (#12), which detects *change* rather than *absolute* OS reservation.
- [ ] Document the second-order concerns of "automatically rolling OS reservation into `system-reserved`": it would change the meaning of operator-set `system-reserved` values, would invalidate the `systemReservedCgroup` cgroup-enforcement sizing (cgroup is sized to operator intent, not detected slop), and would silently shrink allocatable when a kernel grows — *the opposite* of the user's stability goal. **The fix must therefore be opt-in.**

### Task 3: Design the in-kubelet feature gate and configuration flag

Per user feedback: if a portable solution exists, the in-kubelet path must be opt-in via a new feature gate **and** a new `KubeletConfiguration` field, to preserve backward compatibility. Even if the "portable" set is "Linux with one of SMBIOS / iomem / persisted-baseline," that is portable enough to merit the feature design. Design the surface; do not implement it in this task.

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "In-Kubelet Feature Design" section).

- [ ] **Feature gate.** Propose name `StableNodeAllocatable` (alternative: `CompensateOSReservedMemory`). Specify:
  - Lifecycle: Alpha (default off) → Beta (default off; users explicitly opt in until accounting churn is well-understood) → GA (default off, deprecate the gate but keep the config field). Beta default-on is **not** appropriate because enabling it changes the numerical value of `allocatable` and could break tightly-sized clusters.
  - Register in `pkg/features/kube_features.go` in alphabetical position, following the template comment at line 31. Add to the version map (line ~1322 / ~1607 area) with the appropriate `featuregate.VersionedSpecs`.
  - Gate scope: enables (a) parsing the new config field, (b) running the detector at kubelet start, (c) folding the detected delta into the effective reservation. With the gate off, kubelet behavior is byte-for-byte identical to today.
- [ ] **`KubeletConfiguration` field.** Propose adding (in both `pkg/kubelet/apis/config/types.go` near line 438 and `staging/src/k8s.io/kubelet/config/v1beta1/types.go`):
  ```go
  // OSReservedMemoryCompensation, if set, instructs kubelet to detect the amount
  // of physical memory the operating system reserved for itself (kernel image,
  // crashkernel, vmemmap, firmware-reserved regions, etc.) and add that amount
  // to the effective memory reservation, so that node allocatable remains stable
  // across kernel upgrades.
  //
  // Requires the StableNodeAllocatable feature gate.
  // +optional
  OSReservedMemoryCompensation *OSReservedMemoryCompensationConfig
  ```
  with a nested config:
  ```go
  type OSReservedMemoryCompensationConfig struct {
      // Sources is the ordered list of detection sources to try.
      // Allowed values: "SMBIOS", "ProcIomem", "PersistedBaseline".
      // Defaults to ["SMBIOS", "PersistedBaseline"].
      Sources []OSReservedMemorySource
      // BaselinePath is where the persisted-baseline fallback stores its data.
      // Defaults to "/var/lib/kubelet/os-reservation-baseline".
      BaselinePath string
      // MaxAdjustmentBytes caps how much memory the detector is allowed to add
      // to the reservation, as a safety guard against bogus readings.
      // Defaults to 2 GiB.
      MaxAdjustmentBytes *resource.Quantity
      // AccountingBucket controls where the detected delta is folded.
      // Allowed values: "SystemReserved" (default), "KubeReserved".
      AccountingBucket OSReservedMemoryBucket
  }
  ```
- [ ] **Semantics with the gate off (default):** field is parsed but ignored (or rejected at validation if non-nil — document the chosen behavior). Capacity/allocatable computation unchanged.
- [ ] **Semantics with the gate on AND field set:** at startup, after `cadvisorInterface.MachineInfo()` returns capacity in `pkg/kubelet/cm/container_manager_linux.go:240-244`, kubelet runs the detector (Task 4), computes `delta = max(0, advertised − MemTotal)` clamped to `MaxAdjustmentBytes`, and adds `delta` to the chosen bucket *inside* the `NodeConfig` passed to `getNodeAllocatableAbsoluteImpl` (`pkg/kubelet/cm/node_container_manager_linux.go:264`). The operator's configured `SystemReserved`/`KubeReserved` numbers are preserved in logs/metrics, but the effective values used in the subtraction at line 269/272 include `delta`. Crucially: **`capacity` itself stays equal to `MemTotal`** — we adjust *reservation*, not capacity, so `validateNodeAllocatable()` at line 316 still works without modification.
- [ ] **Interaction with cgroup enforcement.** `systemReservedCgroup` / `kubeReservedCgroup` sizes are normally pulled from operator config. With this feature, the cgroup limit must use the *adjusted* (operator + detected delta) value; document the change required in `enforceNodeAllocatableCgroups` (`pkg/kubelet/cm/node_container_manager_linux.go`, function near line 30 — locate exact line during implementation) and call out the risk that cgroup enforcement of `system-reserved` will now reserve more cgroup-level memory than the operator literally typed (this is the *correct* behavior, but must be release-noted).
- [ ] **Observability.** Emit a kubelet log at startup with `detected_os_reserved_bytes`, `source`, and `effective_system_reserved_bytes`. Add a Prometheus gauge `kubelet_os_reservation_detected_bytes` and `kubelet_os_reservation_effective_reservation_bytes{bucket="system"|"kube"}` so operators can observe the adjustment and alert on jumps.
- [ ] **Validation.** In the validation file under `pkg/kubelet/apis/config/validation/` (locate exact file during implementation; today the kubelet config validator lives in `pkg/kubelet/apis/config/validation/validation.go`), require:
  - `OSReservedMemoryCompensation` only set when the feature gate is enabled (else `field.Forbidden`).
  - `Sources` not empty; only known values.
  - `MaxAdjustmentBytes` ≥ 0 and ≤ 50% of `MemTotal` (read at validate time) as a sanity cap.
  - Mutually exclusive with `ReservedSystemCPUs` (no — CPU is unaffected; document) and **not** mutually exclusive with `SystemReservedCgroup` (the cgroup must absorb the delta).
- [ ] **Migration & backward compatibility.** Explicitly document:
  - Default behavior: unchanged (gate off → no detection, no adjustment).
  - For existing clusters: enabling the gate without setting the config field is a no-op.
  - For new clusters: clusters bootstrapped with the field set will have stable allocatable across kernel upgrades but a one-time *lower* allocatable than they would have had pre-feature (because the OS reservation is now visibly subtracted). Release notes must call this out so operators size accordingly.
  - For the external-detector path (Task 4): the two approaches are mutually exclusive; using both would double-count. Validation should warn if the field is set AND a config drop-in named `kubelet-config-osreserved.*` exists, but cannot strictly forbid it.

### Task 4: Plan the external detector — reading mechanism only

Per user feedback: the external Linux helper for Ubuntu and COS is *not* being implemented in this PR; we are only planning **how the program reads the OS-reservation information**. No CLI flags, no bootstrap wiring, no systemd units.

**Files:**
- Modify: `docs/design/node-os-reservation.md` (append "External Detector — Reading Plan" section).

- [ ] Describe the reading algorithm (target ≤ 1 page) as ordered, fall-through steps:
  1. **Read `MemTotal`** by parsing `/proc/meminfo` for the `MemTotal:\s*([0-9]+) kB` line. This is the *current* (post-OS-reservation) value and matches what kubelet sees.
  2. **Try SMBIOS first** by opening `/sys/firmware/dmi/tables/DMI` (raw binary blob) and walking the SMBIOS records. For each Type 17 (`Memory Device`) record, read the 2-byte `Size` field at offset 0x0C; if it equals `0x7FFF`, read the 4-byte `Extended Size` at 0x1C (Mebibyte units); skip records with size = 0 (empty slot) or size = 0xFFFF (unknown). Sum sizes across records. Reject if the result is `< MemTotal` or `> 1.25 × MemTotal` (sanity gate). On COS, `/sys/firmware/dmi/tables/DMI` is readable without installing `dmidecode`.
  3. **Fall through to `/proc/iomem`** if SMBIOS unavailable or rejected: open `/proc/iomem`, sum the byte length of every line matching `^[0-9a-f]+-[0-9a-f]+ : System RAM$` (root-equivalent privilege required; if the addresses are all zeros the process lacks `CAP_SYS_RAWIO` and this source must be marked failed). Apply the same `[MemTotal, 1.25 × MemTotal]` sanity gate.
  4. **Fall through to persisted baseline** at `/var/lib/kubelet/os-reservation-baseline`: a single-line text file `version=1 advertised_bytes=<u64> recorded_at=<RFC3339>`. If the file does not exist, write the current `MemTotal` as the baseline and exit with `delta = 0`. If it exists, use the stored value as the advertised total.
  5. Compute `delta = max(0, advertised − MemTotal)`, round **up** to the nearest 16 MiB (so adjustments are stable across small fluctuations), clamp to a configured maximum (default 2 GiB).
- [ ] Describe the **output contract** in one paragraph: the program writes a kubelet config drop-in (`KubeletConfiguration` YAML containing `systemReserved.memory: <base + delta>`) to a configurable output path. The serialization uses `k8s.io/kubelet/config/v1beta1` and `sigs.k8s.io/yaml`. **No further bootstrap or systemd planning** in this PR (per user feedback).
- [ ] Describe the **failure mode**: if no source returns a usable value AND no baseline exists, write a no-op drop-in (operator's `systemReserved.memory` unchanged) and log a warning. **Never** exit non-zero — the bootstrap must always be able to start kubelet.
- [ ] Describe **distro-specific notes** in a short subsection:
  - **Ubuntu (22.04 / 24.04 cloud images)**: SMBIOS path works on GCE/AWS/Azure x86_64. On AWS Graviton (arm64) SMBIOS may be empty — the iomem fallback is needed. `dmidecode` is present in `linux-cloud-tools-common` but is *not* required because we parse the raw blob.
  - **COS**: SMBIOS path works on all GCE machine types we have data for. `dmidecode` is not installed; the raw-blob parser must be self-contained.
- [ ] **Explicitly defer** to future work (out of scope for this PR, but list them so reviewers see the boundary): packaging the binary, the systemd `ExecStartPre=` unit, kube-up/COS configure-helper changes, kubeadm integration, e2e coverage of the external path.

## Questions

1. **Accounting bucket default.** When the feature gate is enabled and the detector returns a delta, should the delta fold into `SystemReserved` or `KubeReserved`?
   - Option A: `SystemReserved` — semantically the OS *is* the system, so this is the most accurate bucket.
   - Option B: `KubeReserved` — operators typically over-provision `KubeReserved`, so adjustments are less likely to push the total over capacity.
   - Option C: A new third bucket `OSReserved` reported separately in node status — most accurate but a larger API surface.
   - **Suggested:** **Option A** (`SystemReserved`) for v1 with a config knob (`AccountingBucket`) to switch to `KubeReserved`. Option C is a follow-up KEP.

2. **Default `Sources` list.** What's the safest default ordering when the operator enables the feature without specifying `Sources`?
   - Option A: `["SMBIOS", "PersistedBaseline"]` — skip `/proc/iomem` because it requires `CAP_SYS_RAWIO`, and kubelet may or may not have it.
   - Option B: `["SMBIOS", "ProcIomem", "PersistedBaseline"]` — try harder before falling back.
   - **Suggested:** **Option A** — the privilege uncertainty around `/proc/iomem` makes it surprising as a default; operators who want it can list it explicitly.

3. **Feature-gate naming.** `StableNodeAllocatable` vs `CompensateOSReservedMemory` vs `DetectOSReservedMemory`.
   - **Suggested:** **`StableNodeAllocatable`** — describes the *user-visible* outcome (stable allocatable across kernel upgrades) rather than the implementation mechanism, which is the convention for kubelet feature gates intended for operators to enable.

4. **Should validation forbid setting the field when the gate is off, or silently ignore it?**
   - Option A: Forbid (`field.Forbidden`) — clear error, no surprise behavior.
   - Option B: Silently ignore with a warning log — eases rollout because clusters can pre-stage the config and flip the gate later.
   - **Suggested:** **Option B** (silent ignore + warning log) so operators can roll out the config and the gate independently, matching the pattern used for several other kubelet alpha features.

5. **Where in this repo should the external detector live, eventually?** (Not implemented in this PR, but reviewers will ask.)
   - Option A: `cmd/kubelet-os-reservation-detector/` + `pkg/kubeletosreservation/` in k/k.
   - Option B: A separate repo under `kubernetes-sigs/`.
   - Option C: Folded into an existing node-side tool (e.g. extend `kubeadm`'s preflight).
   - **Suggested:** **Option A** — keeps the implementation compile-time-checked against `k8s.io/kubelet/config/v1beta1` and avoids a release-train split. Defer the decision to the follow-up PR.
