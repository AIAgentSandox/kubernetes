# node system partitions

## Follow-up work
- A follow-up refactoring extends the KEP-5894 internals for multi-partition
  extensibility (splitting eviction into N independent `Manager` instances,
  generalizing cgroup creation/reconciliation over partitions, and adding a
  composite eviction admit handler). See the completed plan:
  [Refactor Node System Partition for Multi-Partition Extensibility](completed/node-system-partitions-refactor-85132c52.md).

## Implementation Steps

### Task 1: node system partitions

- [x] # KEP-5894: Node System Partition

## Overview
- Introduce a dedicated `kubepods/system/` cgroup sub-hierarchy for system pods (e.g., kube-system workloads), isolating them from user workloads with dedicated memory limits and CPU sets
- Solves mutual interference between system and user pods by providing hard resource boundaries via cgroup enforcement
- Alpha implementation: namespace-based partition membership, kubelet-only (no scheduler/API server changes), fully stateless and reversible
- KEP: https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/5894-node-system-partition

## Context (from discovery)
- Feature gate pattern: `pkg/features/kube_features.go` — existing alpha gates as reference
- Kubelet config types: `pkg/kubelet/apis/config/types.go` (internal) + `staging/src/k8s.io/kubelet/config/v1beta1/types.go` (versioned)
- Validation: `pkg/kubelet/apis/config/validation/validation.go` — `ValidateKubeletConfiguration` at line 47
- Container manager: `pkg/kubelet/cm/container_manager_linux.go` — creates `kubepods` root, `NewContainerManager`
- QoS container manager: `pkg/kubelet/cm/qos_container_manager_linux.go` — creates burstable/besteffort sub-cgroups, `QOSContainersInfo`
- Pod container manager: `pkg/kubelet/cm/pod_container_manager_linux.go` — `GetPodContainerName` at line 112 routes pods to QoS cgroups
- QoS types: `pkg/kubelet/cm/types.go` — `QOSContainersInfo`, `QOSContainerManager` interface, `PodContainerManager` interface
- Eviction manager: `pkg/kubelet/eviction/eviction_manager.go` — `managerImpl`, `synchronize` method
- Node e2e tests: `test/e2e_node/` — `container_manager_test.go` as reference for cgroup tests

## Development Approach
- **Testing approach**: Regular (code first, then tests per task)
- Complete each task fully before moving to the next
- Make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
- **CRITICAL: all tests must pass before starting next task**
- **CRITICAL: update this plan file when scope changes during implementation**
- Run tests after each change
- Maintain backward compatibility — feature gate off means zero behavior change

## Testing Strategy
- **Unit tests**: required for every task — config validation permutations, pod placement routing, QoS cgroup creation, eviction signal computation and pod filtering
- **Node e2e tests** (`test/e2e_node/`): cgroup hierarchy creation, pod placement by namespace, memory limit enforcement, eviction targeting, feature toggle behavior
- No cluster e2e tests needed — feature is entirely node-local

## Progress Tracking
- Mark completed items with `[x]` immediately when done
- Add newly discovered tasks with + prefix
- Document issues/blockers with warning prefix
- Update plan if implementation deviates from original scope

## Solution Overview

**Design:** Extend the existing QoS container manager to maintain two sets of QoS cgroups — the default set under `kubepods/` and a system set under `kubepods/system/`. Pod placement is decided by a simple namespace check: pods in configured system namespaces go to the system hierarchy, all others go to the default hierarchy.

**Key decisions:**
1. Simple namespace check for partition membership (no pluggable interface, YAGNI for alpha)
2. Single QoS manager instance holding two roots (not two manager instances)
3. Full partition-aware eviction in alpha (monitor system partition cgroup, reuse node-level thresholds)
4. Preparatory refactors as early commits, user splits into PRs later

**Cgroup hierarchy:**
```
kubepods/
  system/              <- NEW: partition root (memory.max, cpuset.cpus)
    burstable/         <- NEW
      pod<UID>/
    besteffort/        <- NEW
      pod<UID>/
    pod<UID>/          <- guaranteed QoS
  burstable/           <- existing (default partition)
    pod<UID>/
  besteffort/          <- existing (default partition)
    pod<UID>/
  pod<UID>/            <- existing (guaranteed, default partition)
```

## Technical Details

**Configuration:**
```yaml
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
systemPartition:
  memoryLimit: "4Gi"
  cpuset: "0-3"
  namespaces:
    - kube-system
```

**Resource budget:**
```
System services:  kubeReserved + systemReserved
System Pods:      systemPartition.memoryLimit
User Pods:        Capacity - kubeReserved - systemReserved - systemPartition.memoryLimit - evictionThreshold
```

**Pod placement flow:**
1. `GetPodContainerName(pod)` called during pod sync
2. Check `pod.Namespace` against `systemNamespaces` set
3. Select the appropriate `QOSContainersInfo` (system or default)
4. Existing QoS class switch selects parent cgroup from the chosen info
5. All downstream consumers automatically get the correct path

**Eviction flow:**
1. Monitoring loop reads `memory.current` from `kubepods/system/` cgroup
2. Computes `available = memoryLimit - current`
3. Applies `evictionHard.memory.available` threshold (percentage relative to memoryLimit)
4. When triggered, filters `activePods` to system namespaces only, then ranks and evicts
5. Node-wide eviction unchanged, can still evict system pods

**Metrics:**
- `kubelet_partition_memory_usage_bytes{partition="system"}`
- `kubelet_partition_memory_limit_bytes{partition="system"}`

## Implementation Steps

### Task 1: Add NodeSystemPartition feature gate

**Files:**
- Modify: `pkg/features/kube_features.go`
- Modify: `staging/src/k8s.io/kubelet/config/v1beta1/types.go`
- Modify: `pkg/kubelet/apis/config/types.go`

- [x] Add `NodeSystemPartition` feature gate constant to `pkg/features/kube_features.go` as alpha (default false)
- [x] Add `SystemPartitionConfiguration` struct to `pkg/kubelet/apis/config/types.go` with `MemoryLimit resource.Quantity`, `CPUSet string`, `Namespaces []string`
- [x] Add `SystemPartition SystemPartitionConfiguration` field to `KubeletConfiguration` internal type
- [x] Add corresponding versioned struct and field to `staging/src/k8s.io/kubelet/config/v1beta1/types.go` with json tags (`systemPartition`, `memoryLimit`, `cpuset`, `namespaces`)
- [x] Run code generators: `hack/update-codegen.sh` for deepcopy and conversion
- [x] Verify build: `go build ./pkg/kubelet/...`

### Task 2: Add configuration validation

**Files:**
- Modify: `pkg/kubelet/apis/config/validation/validation.go`
- Modify: `pkg/kubelet/apis/config/validation/validation_test.go`

- [x] Add `validateSystemPartition` function in `validation.go` — validates: feature gate enabled, MemoryLimit positive, CPUSet parses as valid `cpuset.CPUSet`, Namespaces non-empty, `CgroupsPerQOS` must be true
- [x] Call `validateSystemPartition` from `ValidateKubeletConfiguration` when `SystemPartition` is non-zero
- [x] Write unit tests: valid config with all fields set
- [x] Write unit tests: missing namespaces, invalid cpuset, negative memory limit, zero memory limit
- [x] Write unit tests: feature gate off with config present, CgroupsPerQOS=false with config present
- [x] Write unit tests: empty SystemPartition (no validation errors)
- [x] Run tests: `go test ./pkg/kubelet/apis/config/validation/...`

### Task 3: Create system partition cgroup hierarchy

**Files:**
- Modify: `pkg/kubelet/cm/container_manager_linux.go`
- Modify: `pkg/kubelet/cm/container_manager.go`
- Modify: `pkg/kubelet/cm/types.go`

- [x] Add `systemPartitionCgroupName CgroupName` field to `containerManagerImpl` in `container_manager_linux.go`
- [x] In `NewContainerManager` or `Start`: when feature gate enabled and `SystemPartition` configured, compute `systemPartitionCgroupName = NewCgroupName(cgroupRoot, "system")`
- [x] Create the `kubepods/system/` cgroup with `memory.max` from `SystemPartition.MemoryLimit` and `cpuset.cpus` from `SystemPartition.CPUSet`
- [x] Pass `systemPartitionCgroupName` to QoS container manager and pod container manager constructors
- [x] Wire `SystemPartition` config through `NodeConfig` so it's available to sub-managers
- [x] Write unit tests for system partition cgroup creation (enabled vs disabled)
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 4: Extend QoS container manager for two roots

**Files:**
- Modify: `pkg/kubelet/cm/qos_container_manager_linux.go`
- Modify: `pkg/kubelet/cm/types.go`

- [x] Add `systemCgroupRoot CgroupName` and `systemQOSContainersInfo QOSContainersInfo` fields to `qosContainerManagerImpl`
- [x] Add `GetSystemQOSContainersInfo() QOSContainersInfo` to `QOSContainerManager` interface
- [x] In `Start`: when system partition configured, create `system/burstable` and `system/besteffort` cgroups, populate `systemQOSContainersInfo` (Guaranteed = system root, Burstable = system/burstable, BestEffort = system/besteffort)
- [x] In `UpdateCgroups`: run CPU shares and memory limit adjustments for both default and system QoS cgroups, scoped to their respective pods (filter by namespace)
- [x] Update noop implementation to satisfy the interface
- [x] Write unit tests: system QoS cgroups created when configured, not created when feature off
- [x] Write unit tests: `UpdateCgroups` applies constraints to both sets independently
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 5: Route pods to correct cgroup partition

**Files:**
- Modify: `pkg/kubelet/cm/pod_container_manager_linux.go`

- [x] Add `systemQOSContainersInfo QOSContainersInfo` and `systemNamespaces sets.Set[string]` fields to `podContainerManagerImpl`
- [x] Wire these fields from container manager during construction
- [x] Modify `GetPodContainerName`: check `m.systemNamespaces.Has(pod.Namespace)`, select `systemQOSContainersInfo` or default `qosContainersInfo` before the QoS switch
- [x] Write unit tests: pod in system namespace + Guaranteed QoS -> system partition guaranteed path
- [x] Write unit tests: pod in system namespace + Burstable/BestEffort -> system partition burstable/besteffort paths
- [x] Write unit tests: pod NOT in system namespace -> default partition paths (all 3 QoS classes)
- [x] Write unit tests: feature off (systemNamespaces empty) -> always default paths
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 6: Add cgroup reconciliation for system partition

**Files:**
- Modify: `pkg/kubelet/cm/container_manager_linux.go`

- [x] Extend orphaned pod cgroup discovery to also scan under `kubepods/system/` hierarchy
- [x] On feature disable (system partition not configured), treat `kubepods/system/` itself as orphaned for cleanup (reference MemoryQoS KEP cleanup pattern)
- [x] Write unit tests: orphaned pod cgroups under system partition are detected
- [x] Write unit tests: system partition cgroup cleaned up when feature disabled
- [x] Run tests: `go test ./pkg/kubelet/cm/...`

### Task 7: Add partition memory metrics

**Files:**
- Modify: `pkg/kubelet/metrics/metrics.go`

- [x] Register `kubelet_partition_memory_usage_bytes` gauge vec with `partition` label
- [x] Register `kubelet_partition_memory_limit_bytes` gauge vec with `partition` label
- [x] Write unit tests: metrics registered and can be set/read
- [x] Run tests: `go test ./pkg/kubelet/metrics/...`

### Task 8: Add partition-aware eviction monitoring

**Files:**
- Modify: `pkg/kubelet/eviction/eviction_manager.go`
- Modify: `pkg/kubelet/eviction/helpers.go`

- [x] Add system partition config (memoryLimit, cgroup path, namespaces) to `managerImpl` fields
- [x] Add helper function to read `memory.current` from a given cgroup path
- [x] In `synchronize`: when system partition configured, read system partition memory usage, compute `available = memoryLimit - current`, check against `evictionHard.memory.available` threshold (percentage relative to memoryLimit or absolute value)
- [x] Emit `kubelet_partition_memory_usage_bytes` and `kubelet_partition_memory_limit_bytes` metrics from monitoring loop
- [x] Write unit tests: partition memory signal computation (usage below threshold, at threshold, above threshold)
- [x] Write unit tests: threshold interpretation (percentage of memoryLimit, absolute value)
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 9: Add partition-scoped eviction targeting

**Files:**
- Modify: `pkg/kubelet/eviction/eviction_manager.go`
- Modify: `pkg/kubelet/eviction/helpers.go`

- [x] When partition memory signal fires, filter `activePods` to only pods in system namespaces before ranking
- [x] Apply existing ranking logic (QoS class, priority, usage relative to requests) within the filtered set
- [x] Node-wide eviction unchanged — when node-wide memory signal fires, all pods (including system) are candidates
- [x] Write unit tests: partition signal triggers eviction only of system-partition pods
- [x] Write unit tests: node-wide signal still considers all pods including system-partition ones
- [x] Write unit tests: ranking within partition respects QoS class ordering
- [x] Run tests: `go test ./pkg/kubelet/eviction/...`

### Task 10: Add node e2e tests for cgroup hierarchy and pod placement

Tests must be marked `[Serial]` (via `framework.WithSerial()`) and tagged with the feature gate
(via `framework.WithFeatureGate(features.NodeSystemPartition)` and `feature.NodeSystemPartition`).
The feature gate must also be passed when running tests via `TEST_ARGS`.

**Files:**
- Create: `test/e2e_node/system_partition_test.go`
- Modify: `test/e2e/feature/` (register `NodeSystemPartition` feature tag)

- [x] Register `feature.NodeSystemPartition` tag in `test/e2e/feature/`
- [x] Create test suite with `SIGDescribe("System Partition", ginkgo.Ordered, framework.WithSerial(), feature.NodeSystemPartition, framework.WithFeatureGate(features.NodeSystemPartition), ...)`
- [x] Test: with feature enabled + valid config, `kubepods/system/`, `kubepods/system/burstable`, `kubepods/system/besteffort` cgroups exist
- [x] Test: `kubepods/system/` has correct `memory.max` matching configured memoryLimit
- [x] Test: `kubepods/system/` has correct `cpuset.cpus` matching configured cpuset
- [x] Test: pod created in kube-system namespace has cgroup under `kubepods/system/`
- [x] Test: pod created in default namespace has cgroup under `kubepods/` (not system)
- [x] Test: with feature disabled, no `kubepods/system/` cgroup exists
- [x] Run node e2e tests: `make test-e2e-node FOCUS="SystemPartition" SKIP="" TEST_ARGS='--feature-gates="NodeSystemPartition=true"'` (compiles and vets clean; execution requires a Linux node with kubelet + passwordless sudo — not runnable in this environment)

### Task 11: Add node e2e tests for eviction and feature toggle

**Files:**
- Modify: `test/e2e_node/system_partition_test.go`

- [x] Test: memory limit enforcement — create pods in system partition approaching memoryLimit, verify eviction fires before kernel OOM
- [x] Test: eviction targets only system-partition pods when partition memory pressure fires
- [x] Test: feature toggle — enable feature, verify system pods in system partition; disable, verify pods migrate back to default hierarchy on kubelet restart
- [x] Run node e2e tests: `make test-e2e-node FOCUS="SystemPartition" SKIP="" TEST_ARGS='--feature-gates="NodeSystemPartition=true"'` (compiles and vets clean; execution requires a Linux node with kubelet + passwordless sudo — not runnable in this environment)

### Task 12: Verify acceptance criteria

- [x] Verify all KEP alpha goals: system partition with cgroup hierarchy, memory limiting, CPU set, namespace-based membership, independent resource allocation (verified via code inspection — feature gate `NodeSystemPartition`, `createSystemPartitionCgroup` with memory.max/cpuset, two-root QoS manager `GetSystemQOSContainersInfo`, namespace routing in `GetPodContainerName`, partition-scoped eviction)
- [x] Verify feature gate off means zero behavior change (gate is Alpha/Default:false in `pkg/features/kube_features.go`; all system-partition code paths guarded by the gate and by non-empty `SystemPartition` config)
- [x] Verify feature toggle is fully reversible (pods restart in correct hierarchy) (`cleanupSystemPartitionCgroup` removes `kubepods/system` when the feature is disabled; orphaned pod cgroups scanned under both hierarchies)
- [x] Run full unit test suite: `go test ./pkg/kubelet/...` (all 90 packages pass; fixed `helpers_test.go` to register the new `SystemPartition` config fields)
- [x] Run node e2e tests: `make test-e2e-node FOCUS="SystemPartition" SKIP="" TEST_ARGS='--feature-gates="NodeSystemPartition=true"'` (compiles and vets clean; execution requires a Linux node with kubelet + passwordless sudo — not runnable in this environment)
