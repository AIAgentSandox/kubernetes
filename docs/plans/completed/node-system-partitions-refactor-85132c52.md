# Refactor Node System Partition for Multi-Partition Extensibility

## Overview
- Refactor the KEP-5894 Node System Partition internals so that adding future partitions requires minimal structural changes
- The config API surface is unchanged (`SystemPartition` singular struct) — only internals are restructured
- The eviction manager is the biggest piece: split from one manager with embedded partition logic into N independent `Manager` instances (node-wide + one per partition)
- Cgroup creation and reconciliation are generalized so they loop over partitions instead of hardcoding "system"
- QoS container sets and pod routing keep their current two-named-variables pattern (references, not instances)

## Context (from discovery)
- Eviction manager: `pkg/kubelet/eviction/eviction_manager.go` — `managerImpl` has `systemPartition`, `partitionMemoryReader`, `partitionMemoryPressure` fields; `synchronizePartitionMemory()` and `partitionMemoryEviction()` methods interleaved into main sync loop
- Eviction partition helpers: `pkg/kubelet/eviction/partition.go` — `partitionMemoryAvailable`, `partitionMemoryThresholdMet`, `filterPodsByNamespaces`, `memoryAvailableHardThreshold`
- Eviction types: `pkg/kubelet/eviction/types.go` — `SystemPartitionConfig`, `Manager` interface, `Config.SystemPartition`
- Container manager: `pkg/kubelet/cm/container_manager_linux.go` — `createSystemPartitionCgroup()`, `cleanupSystemPartitionCgroup()`, `systemPartitionCgroupName` field
- Pod container manager: `pkg/kubelet/cm/pod_container_manager_linux.go` — `systemQOSContainersForScan()` for orphan scanning
- Kubelet wiring: `pkg/kubelet/kubelet.go:1105` — `eviction.NewManager()`, `klet.evictionManager` (singular field)
- Metrics: `pkg/kubelet/metrics/metrics.go` — `PartitionMemoryUsage`, `PartitionMemoryLimit` GaugeVec with `partition` label (already generalizable)

## Design Decisions (from brainstorm)

**Rule for data structures:**
1. If we can hold two of something (eviction managers, cgroup roots for scanning), keep them in a `map[string]...` keyed by partition name
2. If we need to reference "default" or "system" (QoS containers, namespace sets), keep two separate named variables for now
3. Each object should enclose enough information to understand its partition

**Eviction manager decomposition:**
- Create separate full `Manager` instances per partition — kubelet starts N eviction managers
- Each is a full `Manager` with its own `Start()` goroutine, sync loop, thresholds, and pod filter
- Stats provider: narrower `PartitionStatsProvider` interface that reads cgroup stats directly (not a wrapper around full node `SummaryProvider`)
- Pod filtering: each partition manager gets an `ActivePodsFunc` that filters by namespace
- Admission: ONE composite admit handler dispatches to the right partition's eviction manager
- Node conditions: only node-wide manager sets `MemoryPressure`/`DiskPressure`/`PIDPressure` — partition pressure is internal only. Add TODO comment that partition pressure should be exposed as a condition in the future
- Metrics: each partition manager emits its own `kubelet_partition_memory_*` metrics using its partition name

**Cgroup creation:** Extract into generic `createPartitionCgroup(name, memoryLimit, cpuset)` helper

**Cgroup reconciliation:** Hold partition roots in `map[string]CgroupName` so orphan scanner iterates

**QoS manager / pod routing:** No changes — keep two named variables per the brainstorm rule

## Development Approach
- **Testing approach**: Regular (code first, then tests per task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility — feature gate off means zero behavior change
- This is a pure refactoring — no user-visible behavior changes

## Testing Strategy
- **Unit tests**: required for every task — partition eviction manager creation, composite admit handler, partition stats provider, cgroup creation helper
- **Existing tests**: all existing eviction and container manager tests must continue to pass
- **Node e2e tests** (`test/e2e_node/system_partition_test.go`): should pass without changes since behavior is unchanged

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with ➕ prefix
- Document issues/blockers with ⚠️ prefix
- Update plan if implementation deviates from original scope

## Solution Overview

**Before (current):**
```
Kubelet
  └── evictionManager (one instance)
        ├── synchronize()           — node-wide eviction
        ├── synchronizePartitionMemory()  — reads system partition cgroup
        └── partitionMemoryEviction()     — evicts system partition pods
```

**After (refactored):**
```
Kubelet
  ├── evictionManager (node-wide)  — Manager interface, no partition knowledge
  ├── partitionEvictionManagers map[string]Manager
  │     └── "system" — full Manager instance, own Start() loop
  └── compositeAdmitHandler — dispatches to correct manager per pod namespace
```

Each partition eviction manager is configured with:
- A `PartitionStatsProvider` scoped to its cgroup root
- Thresholds (reuses node-level hard thresholds for now)
- An `ActivePodsFunc` filtering by partition namespaces
- Its partition name (for metrics and logging)

## Technical Details

**PartitionStatsProvider interface:**
```go
// PartitionStatsProvider provides resource usage stats for a single partition.
type PartitionStatsProvider interface {
    // GetPartitionStats returns the current resource usage of the partition.
    GetPartitionStats() (*PartitionStats, error)
}

type PartitionStats struct {
    MemoryUsageBytes uint64
    MemoryLimitBytes uint64
}
```

**Composite admit handler:**
```go
type compositeEvictionAdmitHandler struct {
    nodeManager          Manager
    partitionManagers    map[string]Manager
    podPartitionFunc     func(pod *v1.Pod) string  // returns partition name or ""
}
```
- Checks the node-wide manager for node conditions (existing behavior)
- Additionally checks the target partition's manager for partition pressure
- Returns deny if either says deny

**Partition eviction manager integration with `managerImpl`:**
- The partition manager IS a `managerImpl` — same struct, different config
- `summaryProvider` field gets a `PartitionStatsProvider` adapter (wraps partition stats into the `Summary` shape the eviction logic expects)
- `Config.Thresholds` populated from node-level hard thresholds
- No `imageGC`/`containerGC` needed (pass no-op implementations)
- `Start()` runs its own goroutine with the same monitoring interval

## Implementation Steps

### Task 1: Extract generic `createPartitionCgroup` helper

**Files:**
- Modify: `pkg/kubelet/cm/container_manager_linux.go`

- [x] Rename `createSystemPartitionCgroup` to `createPartitionCgroup` — make it a package-level function that takes `(logger, cgroupManager, name, config)` with no "system" in the name
- [x] Update the call site in `containerManagerImpl` to use the renamed function
- [x] Update `cleanupSystemPartitionCgroup` log messages to be partition-name-aware (take partition name as parameter)
- [x] Verify build: `go build ./pkg/kubelet/cm/...`
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 2: Add `map[string]CgroupName` for partition roots in orphan scanning

**Files:**
- Modify: `pkg/kubelet/cm/container_manager_linux.go`
- Modify: `pkg/kubelet/cm/pod_container_manager_linux.go`

- [x] Add `partitionCgroupRoots map[string]CgroupName` field to `containerManagerImpl` alongside existing `systemPartitionCgroupName`
- [x] Populate the map with `"system" -> systemPartitionCgroupName` when system partition is configured
  - ⚠️ Deviation (to preserve behavior): the map ALWAYS includes the system partition scan root even when the feature is disabled — it uses `systemPartitionCgroupName` when configured, otherwise the derived `kubepods/system` location. This keeps the existing rollback-cleanup behavior (leftover pod cgroups under `kubepods/system` are still discovered after the feature is disabled). Populated only when `CgroupsPerQOS` is enabled, since the pod container manager only scans in that mode.
- [x] Pass `partitionCgroupRoots` to `podContainerManagerImpl` for orphan scanning
- [x] Refactor `systemQOSContainersForScan` to iterate over `partitionCgroupRoots` map instead of hardcoding the "system" partition name
  - Renamed to `partitionQOSContainersForScan()` returning `[]QOSContainersInfo`; QoS names are derived from each partition root (Guaranteed==root, Burstable/BestEffort as lowercased children), matching `createQOSContainers` exactly, so behavior is unchanged. `qosContainersToScan()` now appends every partition's QoS containers.
- [x] Write unit tests: orphan scanning finds cgroups under all partition roots in the map (`TestGetAllPodsFromCgroupsPartitions`, incl. a synthetic second "other" partition root)
- [x] Write unit tests: empty map means no partition cgroups scanned (`TestGetAllPodsFromCgroupsPartitions` empty-map case scans only the default partition)
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 3: Define `PartitionStatsProvider` interface and implementation

**Files:**
- Create: `pkg/kubelet/eviction/partition_stats.go`
- Create: `pkg/kubelet/eviction/partition_stats_test.go`

- [x] Define `PartitionStatsProvider` interface with `GetPartitionStats() (*PartitionStats, error)` method
- [x] Define `PartitionStats` struct with `MemoryUsageBytes uint64`, `MemoryLimitBytes uint64`
- [x] Implement `cgroupPartitionStatsProvider` that reads memory usage from cgroup filesystem using the existing `readPartitionMemoryUsage` function and gets limit from config
- [x] Write unit tests: stats provider returns correct values (mock the cgroup read)
- [x] Write unit tests: stats provider returns error when cgroup read fails
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 4: Create partition eviction manager factory

**Files:**
- Modify: `pkg/kubelet/eviction/eviction_manager.go`
- Modify: `pkg/kubelet/eviction/types.go`

- [x] Add `NewPartitionManager` constructor that creates a `managerImpl` configured for partition-scoped eviction: takes partition name, `PartitionStatsProvider`, thresholds, `KillPodFunc`, recorder, nodeRef, clock
- [x] The partition manager's `summaryProvider` is an adapter that wraps `PartitionStatsProvider` to produce a minimal `Summary` with just memory stats
  - Adapter is `partitionSummaryProvider` in `partition_stats.go`; shapes `Node.Memory` so `available = limit - usage` (clamped at 0) and `workingSet = usage`, making `available + workingSet == limit`. This lets `makeSignalObservations` derive the `memory.available` signal with capacity equal to the partition limit, so the node-level eviction logic evaluates partition thresholds unchanged.
- [x] The partition manager's `imageGC`/`containerGC` are no-ops (partitions don't manage images)
  - Added `noopImageGC` / `noopContainerGC` types in `eviction_manager.go`.
- [x] Add `partitionName string` field to `managerImpl` for metrics labels and logging
- [x] Add TODO comment on `IsUnderMemoryPressure()`: "TODO(KEP-5894): When multiple partitions are supported, expose partition-level pressure as a node condition or pod condition so schedulers can react"
- [x] Write unit tests: `NewPartitionManager` creates a valid manager with correct config (`TestNewPartitionManager`, `TestNoopGC`)
- [x] Write unit tests: partition manager's stats adapter produces correct memory observations (`TestPartitionSummaryProviderGet`, `TestPartitionSummaryProviderObservations`)
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 5: Remove partition-specific code from node-wide eviction manager

**Files:**
- Modify: `pkg/kubelet/eviction/eviction_manager.go`
- Modify: `pkg/kubelet/eviction/types.go`

- [x] Remove `systemPartition *SystemPartitionConfig` field from `managerImpl`
- [x] Remove `partitionMemoryReader` field from `managerImpl`
- [x] Remove `partitionMemoryPressure bool` field from `managerImpl`
- [x] Remove `synchronizePartitionMemory()` method
- [x] Remove `partitionMemoryEviction()` method
- [x] Remove `IsUnderPartitionMemoryPressure()` method
- [x] Remove the `synchronizePartitionMemory` call from `synchronize()` (line 390)
- [x] Remove the `partitionMemoryEviction` call from `synchronize()` (line 517)
- [x] Remove `SystemPartition` field from `Config` struct in `types.go`
- [x] Remove `SystemPartitionConfig` struct from `types.go` (moved to partition_stats.go or kept in partition.go)
  - Removed entirely; `partition_stats.go` already carries its own `cgroupPath`/`memoryLimit` fields and does not need the struct. Also dropped the now-unused `sets` import from `types.go`.
- [x] Update `NewManager` to no longer accept or store partition config
  - `NewManager` signature unchanged (still takes `Config`), but no longer reads `config.SystemPartition` or seeds `partitionMemoryReader`.
- [x] Update existing tests that reference removed fields/methods
  - Removed `TestPartitionMemoryEviction`, `TestPartitionMemoryEvictionNodeWideIncludesSystemPods`, and `TestSynchronizePartitionMemory` from `partition_test.go` (they exercised the removed node-wide partition path; equivalent coverage now lives in the partition-manager tests). Trimmed the unused imports. Removed the `impl.systemPartition` nil-check from `partition_manager_test.go`.
  - ⚠️ Deviation (to keep the tree compiling): removed the `evictionConfig.SystemPartition` population block in `pkg/kubelet/kubelet.go` (it referenced the now-deleted `eviction.SystemPartitionConfig`) and replaced it with a `TODO(KEP-5894)` marking where Task 7 will construct per-partition `eviction.Manager` instances. Partition-scoped eviction is temporarily inert between Task 5 and Task 7; Task 7 re-wires it.
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 6: Create composite eviction admit handler

**Files:**
- Create: `pkg/kubelet/eviction/composite_admit.go`
- Create: `pkg/kubelet/eviction/composite_admit_test.go`

- [x] Define `compositeEvictionAdmitHandler` struct with `nodeManager Manager`, `partitionManagers map[string]Manager`, `podPartitionFunc func(*v1.Pod) string`
  - ⚠️ Deviation: the node field is `nodeAdmitHandler lifecycle.PodAdmitHandler`, not `nodeManager Manager`. The `Manager` interface does not expose `Admit`, and preserving the existing node-wide admission behavior (critical exempt, best-effort/toleration nuance) requires the full admit logic returned by `NewManager`, so the composite holds the node admit handler directly. `partitionManagers map[string]Manager` and `podPartitionFunc` match the plan.
- [x] Implement `Admit(ctx, attrs)` — check node-wide conditions first (existing logic), then check if the pod's target partition is under pressure
  - Node-wide rejection wins; then critical pods are always admitted; then the pod's target partition (via `podPartitionFunc`) is rejected only when that partition's `Manager.IsUnderMemoryPressure()` is true. Empty partition name or unknown partition ⇒ admit.
- [x] The `podPartitionFunc` returns partition name for system pods, empty string for default pods
- [x] Write unit tests: node pressure rejects all non-critical pods (existing behavior preserved) (`TestCompositeEvictionAdmitHandlerNodePressureRejectsNonCriticalPods`, `...NodePressureAdmitsCriticalPods`)
- [x] Write unit tests: partition pressure rejects only pods targeting that partition (`TestCompositeEvictionAdmitHandlerPartitionPressureRejectsOnlyPartitionPods`)
- [x] Write unit tests: no pressure admits all pods (`TestCompositeEvictionAdmitHandlerNoPressureAdmitsAllPods`)
- [x] Write unit tests: critical pods always admitted regardless of partition pressure (`TestCompositeEvictionAdmitHandlerCriticalPodsAlwaysAdmittedUnderPartitionPressure`); plus `...UnknownPartitionAdmits` for the missing-manager path
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 7: Wire partition eviction managers in kubelet

**Files:**
- Modify: `pkg/kubelet/kubelet.go`
- Modify: `pkg/kubelet/kubelet_pods.go` (if needed for filtered pod func)

- [x] Add `partitionEvictionManagers map[string]eviction.Manager` field to `Kubelet` struct
  - Also added `partitionNamespaces map[string]sets.Set[string]` so `Run()` can build each partition manager's namespace-filtered `ActivePodsFunc` and the composite admit handler can route pods.
- [x] When system partition is configured, create a partition eviction manager via `eviction.NewPartitionManager`
  - Extracted into a testable package-level helper `newPartitionEvictionManagers(systemPartition, cgroupPath, thresholds, killPodFunc, recorder, nodeRef, clock)` in `kubelet.go`. It gates on the `NodeSystemPartition` feature gate, non-empty partition namespaces, and a non-empty `GetSystemPartitionCgroupRoot()` (preserving the prior "don't monitor the memory cgroup root" safeguard). Added exported `eviction.NewCgroupPartitionStatsProvider`, `eviction.SystemPartitionName`, and `eviction.PartitionThresholds` (returns only the hard `memory.available` threshold, matching the pre-refactor partition eviction trigger and excluding soft/disk/PID thresholds the partition summary cannot evaluate).
- [x] Create `compositeEvictionAdmitHandler` wrapping node-wide + partition managers, register as admit handler
  - Registered via exported `eviction.NewCompositeEvictionAdmitHandler` only when at least one partition manager exists; otherwise the node-wide `evictionAdmitHandler` is registered as before (zero behavior change when the feature is off). `podPartitionFunc` maps a pod's namespace to its partition name.
- [x] In `Run()`, call `Start()` on each partition eviction manager with a namespace-filtered `ActivePodsFunc`
  - Each partition manager starts right after the node-wide `evictionManager.Start`, with an `ActivePodsFunc` that filters `getAllocatedPods()` by the partition's namespace set, and reuses `kl.StatsProvider` as the `DiskInfoProvider` and `kl.PodIsFinished` as the cleanup func.
- [x] Remove `SystemPartition` from `evictionConfig` passed to `NewManager` (it no longer uses it)
  - Already removed in Task 5; the Task-5 TODO placeholder in `kubelet.go` is now replaced by the real wiring.
- [x] Write unit tests: partition manager is created when system partition is configured (`TestNewPartitionEvictionManagers` "system partition configured" case)
- [x] Write unit tests: partition manager is NOT created when feature gate is off (`TestNewPartitionEvictionManagers` "feature disabled" case; plus "no partition namespaces configured" and "partition cgroup does not exist" negative cases). Also added `TestPartitionThresholds` covering the hard-threshold selection.
- [x] Run tests: `go test ./pkg/kubelet/...`

### Task 8: Clean up partition.go — keep only reusable helpers

**Files:**
- Modify: `pkg/kubelet/eviction/partition.go`
- Modify: `pkg/kubelet/eviction/partition_test.go`

- [x] Keep `filterPodsByNamespaces` (used by composite admit handler and pod func filtering)
- [x] Keep `partitionMemoryAvailable`, `partitionMemoryThresholdMet`, `memoryAvailableHardThreshold` (used by partition stats/eviction logic)
- [x] Remove `systemPartitionName` constant (each manager carries its own partition name)
  - It was unused: `partition_stats.go` already defines the exported `SystemPartitionName` used everywhere. Removed the dead `systemPartitionName` constant from `partition.go`.
- [x] Remove `partitionMemoryReader` type if moved to `partition_stats.go`
  - Moved the `partitionMemoryReader` type definition from `partition.go` to `partition_stats.go`, next to its sole consumer `cgroupPartitionStatsProvider.readMemoryUsage`.
- [x] Update tests to match relocated/removed symbols
  - `partition_test.go` only exercises the kept helpers (`partitionMemoryAvailable`, `partitionMemoryThresholdMet`, `memoryAvailableHardThreshold`, `filterPodsByNamespaces`); no test referenced the removed constant/relocated type, so no test changes were required. Verified with a package-wide grep.
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 9: Verify acceptance criteria

- [x] Verify all existing unit tests pass: `go test ./pkg/kubelet/eviction/...` (pass)
- [x] Verify all container manager tests pass: `go test ./pkg/kubelet/cm/...` (pass)
- [x] Verify full kubelet test suite: `go test ./pkg/kubelet/...` (pass, no failures)
- [x] Verify feature gate off means zero behavior change (no partition manager created, no partition cgroups) — `newPartitionEvictionManagers` returns empty maps when `NodeSystemPartition` is disabled (kubelet.go:392); composite admit handler is only registered when `len(partitionEvictionManagers) > 0` (kubelet.go:1156), otherwise the node-wide `evictionAdmitHandler` is used as before
- [x] Verify node e2e tests compile: `go vet ./test/e2e_node/` passes (compile check; full `make test-e2e-node` execution requires a Linux node)
- [x] Verify TODO comment exists for future partition pressure reporting — `TODO(KEP-5894)` on `IsUnderMemoryPressure()` at eviction_manager.go:290

### Task 10: [Final] Update documentation

- [x] Update `docs/plans/node-system-partitions-85132c52.md` to reference this refactoring plan
- [x] Move this plan to `docs/plans/completed/` when all tasks are done (copied to `docs/plans/completed/node-system-partitions-refactor-85132c52.md`)

## Post-Completion
*Items requiring manual intervention or external systems — no checkboxes, informational only*

**Manual verification:**
- Run node e2e tests on a Linux node: `make test-e2e-node FOCUS="SystemPartition" SKIP="" TEST_ARGS='--feature-gates="NodeSystemPartition=true"'`
- Verify partition eviction still fires correctly under memory pressure
- Verify metrics `kubelet_partition_memory_usage_bytes` and `kubelet_partition_memory_limit_bytes` are still emitted with correct labels
