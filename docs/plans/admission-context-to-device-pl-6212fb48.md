# Propagate `context.Context` through Pod Admission to Device Plugin

## Overview

Propagate `context.Context` from the kubelet's pod-admission entry points all the way down to the device-plugin gRPC calls in the Device Manager. This is the successor to abandoned PR [#128008](https://github.com/kubernetes/kubernetes/pull/128008): much of the upper-layer threading (Kubelet `HandlePodAdditions`, TopologyManager `Scope.Admit`, device-manager endpoint RPCs, `GetDeviceRunContainerOptions`, `allocateContainerResources`) has since been merged piecemeal, but the critical bridges in the middle still create `context.TODO()` / `klog.TODO()` locally. The remaining work is to plumb `ctx` through:

1. `lifecycle.PodAdmitHandler.Admit` (the interface that turns admission into a context-aware operation).
2. `cm/{cpumanager,memorymanager,devicemanager}` `Manager` methods used during admission (`Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`).
3. The `topologymanager.HintProvider` interface (`GetTopologyHints`, `GetPodTopologyHints`, `Allocate`, `AllocatePod`).
4. The `allocation.Manager` admission paths (`AddPod`, `handlePodResourcesResize`, `canAdmitPod`).

Per the task description, where a method currently accepts an explicit `klog.Logger` / `logr.Logger` as its only "context-carrier" parameter and is being given a `ctx`, replace the logger parameter with `ctx` (extracting the logger via `klog.FromContext(ctx)` inside the function) rather than passing both. Where a method is reached from a non-admission path that has only a logger today (e.g. `AddContainer`, `RemoveContainer`, policy `Start`, reconcile loops), keep the existing logger signature — this PR scopes the change to the admission → device-plugin chain.

## Context

- **Files involved** (top-level — full list in each task):
  - `pkg/kubelet/lifecycle/interfaces.go`, `predicate.go`, `handlers.go`
  - `pkg/kubelet/eviction/eviction_manager.go`
  - `pkg/kubelet/sysctl/allowlist.go`
  - `pkg/kubelet/nodeshutdown/nodeshutdown_manager.go`, `nodeshutdown_manager_linux.go`
  - `pkg/kubelet/cm/topologymanager/{topology_manager,scope,scope_container,scope_pod,scope_none,fake_topology_manager}.go`
  - `pkg/kubelet/cm/cpumanager/{cpu_manager,policy,policy_none,policy_static,fake_cpu_manager}.go`
  - `pkg/kubelet/cm/memorymanager/{memory_manager,policy,policy_none,policy_static,policy_best_effort,fake_memory_manager}.go`
  - `pkg/kubelet/cm/devicemanager/{manager,topology_hints,types}.go`
  - `pkg/kubelet/cm/container_manager_windows.go`
  - `pkg/kubelet/allocation/{allocation_manager,handlers}.go`
  - `pkg/kubelet/kubelet.go`
  - Tests and fake implementations across the same packages.

- **Related patterns already in the tree**:
  - `kubelet.HandlePodAdditions(ctx context.Context, …)` already passes `ctx` (kubelet.go:2842).
  - `topologymanager.Scope.Admit(ctx context.Context, pod *v1.Pod)` already takes `ctx` (scope.go:45). `topology_manager.go:262` creates `context.TODO()` only because `Manager.Admit` (i.e. `PodAdmitHandler.Admit`) does not yet accept one — the TODO is in-place.
  - Device manager endpoints (`endpoint.allocate`, `endpoint.getPreferredAllocation`, `endpoint.preStartContainer`) already accept `ctx` (endpoint.go:32–40).
  - `ManagerImpl.allocateContainerResources`, `devicesToAllocate`, `callGetPreferredAllocationIfAvailable`, `callPreStartContainerIfNeeded`, and `GetDeviceRunContainerOptions` already accept `ctx` (manager.go:868, 611, 1059, 1025, 987).
  - `predicate.go:122`, `topology_manager.go:265`, and `devicemanager/manager.go:398` each carry a `// TODO: pass context …` / `// Use context.TODO()` comment marking the exact insertion points that this plan resolves.

- **Dependencies**: `k8s.io/kubernetes/test/utils/ktesting` (`ktesting.NewTestContext(t)`) for test setup, already used throughout `pkg/kubelet/`.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices — every package touched in this plan already has table-driven tests using `ktesting.NewTestContext(t)` (CPU/memory/device managers, topology manager, eviction, sysctl, nodeshutdown, allocation, kubelet). Update the existing tests to pass `ctx` where signatures change; do not invent new test harnesses.
- Complete each task fully (signature change + all callers + all tests + `go build ./... && go test ./pkg/kubelet/...` for the affected packages) before moving to the next.
- **CRITICAL: all tests must pass before starting next task** — including `go vet ./pkg/kubelet/...` and the relevant package's unit tests.
- Use `klog.FromContext(ctx)` inside any function that previously had `logger := klog.TODO()` and is now given a `ctx`. Do not keep both the logger parameter and ctx unless the function is reached from both an admission path (ctx-aware) and a non-admission path (logger-only).
- Stage the change layer-by-layer (leaves last → roots first is impractical here because all signatures cascade); instead, change one interface plus its implementations and direct callers per task, leaving compilation green at the end of each task. Run `hack/update-codegen.sh` is **not** needed (no generated code changes).
- When updating an interface, also update mocks/fakes in the same task (e.g. `fake_topology_manager.go`, `fake_cpu_manager.go`, `fake_memory_manager.go`, `MockEndpoint` in `manager_test.go`).

## Implementation Steps

### Task 1: Add `ctx context.Context` to `PodAdmitHandler.Admit`

Promote `ctx` to a first-class parameter on the interface that gates all kubelet pod admission. This is the keystone change — every admission handler signature flows from here. Resolve the TODO at `pkg/kubelet/lifecycle/predicate.go:120–123` and `pkg/kubelet/cm/topologymanager/topology_manager.go:263–266` in the same task.

**Files:**
- Modify: `pkg/kubelet/lifecycle/interfaces.go`
- Modify: `pkg/kubelet/lifecycle/predicate.go`
- Modify: `pkg/kubelet/lifecycle/handlers.go` (all `*AdmitHandler.Admit` implementations: `appArmorAdmitHandler`, `podFeaturesAdmitHandler`, `declaredFeaturesAdmitHandler`, and any others present)
- Modify: `pkg/kubelet/eviction/eviction_manager.go` (`managerImpl.Admit` — remove the local `ctx := context.Background()`)
- Modify: `pkg/kubelet/sysctl/allowlist.go` (`patternAllowlist.Admit`)
- Modify: `pkg/kubelet/nodeshutdown/nodeshutdown_manager.go` (`Manager` interface + `managerStub.Admit`)
- Modify: `pkg/kubelet/nodeshutdown/nodeshutdown_manager_linux.go` (`managerImpl.Admit`)
- Modify: `pkg/kubelet/cm/topologymanager/topology_manager.go` (`manager.Admit` — accept `ctx` from caller, drop `context.TODO()`)
- Modify: `pkg/kubelet/cm/topologymanager/fake_topology_manager.go` (`fakeManager.Admit`)
- Modify: `pkg/kubelet/cm/container_manager_windows.go` (`noopWindowsResourceAllocator.Admit`)
- Modify: `pkg/kubelet/allocation/handlers.go` (`podResizesAdmitHandler.Admit` — uses `h.logger`; either keep `h.logger` as fallback for callers without ctx, or extract via `klog.FromContext(ctx)`; prefer the latter and drop the `logger` field)
- Modify: `pkg/kubelet/allocation/allocation_manager.go` (`canAdmitPod` — pass `ctx` into `podAdmitHandler.Admit(ctx, attrs)`; see Task 7)
- Modify: tests for every file above. Each test already calls `Admit(attrs)`; switch to `Admit(ctx, attrs)` using `_, ctx := ktesting.NewTestContext(t)` (the import is already used widely in these packages).

- [ ] Update `PodAdmitHandler.Admit` interface signature in `pkg/kubelet/lifecycle/interfaces.go` to `Admit(ctx context.Context, attrs *PodAdmitAttributes) PodAdmitResult` and add `import "context"`.
- [ ] Update every concrete implementation (8+ types listed above) to accept `ctx` and pass it through. In `predicate.go` replace `ctx := context.TODO()` with the parameter; in `topology_manager.go` `Admit` replace the inline `context.TODO()` and remove the TODO comment at lines 263–264.
- [ ] Update `allocation.manager.canAdmitPod` to accept `ctx` (see Task 7) and pass it into every `podAdmitHandler.Admit(ctx, attrs)` call in the loop.
- [ ] Update every test that calls `.Admit(attrs)` on a `PodAdmitHandler` to pass `ctx` from `ktesting.NewTestContext(t)`. Files include `eviction_manager_test.go`, `nodeshutdown_manager_linux_test.go`, `allowlist_test.go`, `fake_topology_manager_test.go`, `kubelet_test.go` (`testPodAdmitHandler.Admit`), and `allocation/*_test.go`.
- [ ] Run `go build ./pkg/kubelet/... && go test ./pkg/kubelet/lifecycle/... ./pkg/kubelet/eviction/... ./pkg/kubelet/sysctl/... ./pkg/kubelet/nodeshutdown/... ./pkg/kubelet/cm/topologymanager/... ./pkg/kubelet/allocation/...`.

### Task 2: Thread `ctx` through `topologymanager.HintProvider`

The HintProvider interface is the contract between TopologyManager and the CPU/Memory/Device managers. Adding `ctx` here unlocks ctx-aware calls inside `Scope.Admit` (which already has `ctx`) and lets the underlying managers stop fabricating `klog.TODO()`. Per the task description, when a HintProvider method had only a `klog.Logger` parameter (none currently — they take only pod/container), we are adding `ctx`; when a callee inside the manager had `logger klog.Logger`, replace it with `ctx` if the entire call chain is being made ctx-aware.

**Files:**
- Modify: `pkg/kubelet/cm/topologymanager/topology_manager.go` (`HintProvider` interface, lines 106–124)
- Modify: `pkg/kubelet/cm/topologymanager/scope.go` (`allocateAlignedResources`, `allocatePodAlignedResources` — accept `ctx` from `Scope.Admit`; pass to `provider.Allocate` / `provider.AllocatePod`; pass to `admitPolicyNone` which currently has no ctx)
- Modify: `pkg/kubelet/cm/topologymanager/scope_container.go` (`accumulateProvidersHints`, `calculateAffinity` — pass `ctx`; `provider.GetTopologyHints(ctx, …)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_pod.go` (`accumulateProvidersHints`, `calculateAffinity`, `admitUsingContainerResources`, `admitUsingPodResources` — pass `ctx`; `provider.GetPodTopologyHints(ctx, …)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_none.go` (`noneScope.Admit` already takes `ctx`; just forward it)
- Modify: `pkg/kubelet/cm/topologymanager/topology_manager_test.go` (`mockHintProvider` — update signatures; update `TestAdmit` to pass `ctx`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_container_test.go` (`calculateAffinity` calls)
- Modify: `pkg/kubelet/cm/topologymanager/scope_pod_test.go` (`calculateAffinity`, `accumulateProvidersHints` calls)
- Modify: `pkg/kubelet/cm/topologymanager/policy_test.go` (`provider.GetTopologyHints` calls in `testPolicyMerge`)

- [ ] Change `HintProvider` interface (`GetTopologyHints`, `GetPodTopologyHints`, `Allocate`, `AllocatePod`) to take `ctx context.Context` as the first parameter.
- [ ] Update `scope.allocateAlignedResources` to take `ctx`; update `scope.allocatePodAlignedResources` to take `ctx`; update `scope.admitPolicyNone` to take `ctx`; thread `ctx` from `Scope.Admit` through these helpers.
- [ ] Update `containerScope.accumulateProvidersHints` and `calculateAffinity` to take `ctx` (drop `logger klog.Logger` parameter or keep both — keep `logger` because the existing logger plumbing inside is fine; just extract once at the top of `Admit` and pass `ctx` to providers).
- [ ] Update `podScope.accumulateProvidersHints` and `calculateAffinity` similarly.
- [ ] Update `mockHintProvider` and all tests under `pkg/kubelet/cm/topologymanager/` to pass `ctx` from `ktesting.NewTestContext(t)`.
- [ ] Provider implementations (CPU/Memory/Device managers) are updated in Tasks 3–5; for this task, leave their `Manager.GetTopologyHints` etc. signatures temporarily compatible by adding a `ctx context.Context` parameter and discarding it (`_ = ctx`). They will be wired up properly in subsequent tasks. Alternative: do this task in one commit alongside Tasks 3–5; either is fine. Recommended split: complete this task by also doing the minimal interface-signature change on each Manager (just to make compile pass) and defer internal cleanup of `klog.TODO()` to Tasks 3–5.
- [ ] Run `go build ./pkg/kubelet/cm/... && go test ./pkg/kubelet/cm/topologymanager/...`.

### Task 3: Thread `ctx` through CPU Manager admission paths

Eliminate the `klog.TODO()` lines at `pkg/kubelet/cm/cpumanager/cpu_manager.go:270, 289, 358, 366`. The CPU policies currently take `logger logr.Logger`; per the task description, where a function is being given a `ctx` and previously had only a logger parameter for context-like state, replace the logger parameter with `ctx` and extract the logger via `klog.FromContext(ctx)`.

**Files:**
- Modify: `pkg/kubelet/cm/cpumanager/cpu_manager.go` (`Manager` interface methods `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints` — add `ctx`; `manager.*` implementations drop their `klog.TODO()`)
- Modify: `pkg/kubelet/cm/cpumanager/policy.go` (`Policy` interface — `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`: replace `logger logr.Logger` with `ctx context.Context`)
- Modify: `pkg/kubelet/cm/cpumanager/policy_none.go`, `policy_static.go` — update signatures, extract `logger := klog.FromContext(ctx)` at top of each method that needs logging
- Modify: `pkg/kubelet/cm/cpumanager/fake_cpu_manager.go` — update fake to match
- Modify: `pkg/kubelet/cm/cpumanager/cpu_manager_test.go` (`mockPolicy` + all `Allocate`/hint test calls)
- Modify: `pkg/kubelet/cm/cpumanager/policy_none_test.go`, `policy_static_test.go`, `topology_hints_test.go` — pass `ctx`
- Leave alone (do not touch in this task): `AddContainer`, `RemoveContainer`, `policyRemoveContainer*`, `Start`, `reconcileState` — these are not on the admission path; they keep their existing `logger logr.Logger` / `ctx` signatures.

- [ ] `cpu_manager.go` `Manager` interface: `Allocate(ctx, pod, container)`, `AllocatePod(ctx, pod)`, `GetTopologyHints(ctx, pod, container)`, `GetPodTopologyHints(ctx, pod)`.
- [ ] `cpu_manager.go` `manager.*` impls: drop `klog.TODO()`, use `klog.FromContext(ctx)`, pass `ctx` into `m.policy.*` calls.
- [ ] `policy.go` `Policy` interface: replace `logger logr.Logger` with `ctx context.Context` on `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints` (keep `state.State`, `*v1.Pod`, `*v1.Container` arguments).
- [ ] `policy_none.go`, `policy_static.go`: update impls; extract logger via `klog.FromContext(ctx)` inside.
- [ ] `fake_cpu_manager.go`: update fake signatures.
- [ ] `cpu_manager_test.go`, `policy_none_test.go`, `policy_static_test.go`, `topology_hints_test.go`: use `_, ctx := ktesting.NewTestContext(t)` and pass `ctx`.
- [ ] Run `go build ./pkg/kubelet/cm/cpumanager/... && go test ./pkg/kubelet/cm/cpumanager/...`.

### Task 4: Thread `ctx` through Memory Manager admission paths

Mirror Task 3 for the memory manager. Eliminate `klog.TODO()` at `pkg/kubelet/cm/memorymanager/memory_manager.go:268, 283`. Memory policies currently take `klog.Logger` — replace with `ctx`.

**Files:**
- Modify: `pkg/kubelet/cm/memorymanager/memory_manager.go` (`Manager` interface `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`)
- Modify: `pkg/kubelet/cm/memorymanager/policy.go` (`Policy` interface — replace `klog.Logger` with `ctx`)
- Modify: `pkg/kubelet/cm/memorymanager/policy_none.go`, `policy_static.go`, `policy_best_effort.go` — update signatures
- Modify: `pkg/kubelet/cm/memorymanager/fake_memory_manager.go`
- Modify: `pkg/kubelet/cm/memorymanager/memory_manager_test.go` (`mockPolicy`, `TestAddContainer`, `TestGetTopologyHints`, `TestAllocateAndAddPodWithInitContainers`, etc.)
- Modify: `pkg/kubelet/cm/memorymanager/policy_static_test.go`
- Leave alone: `GetMemoryNUMANodes`, `RemoveContainer`, `AddContainer` (non-admission, keep `klog.Logger` param)

- [ ] Update `Manager` interface and `manager.*` impls.
- [ ] Update `Policy` interface (Allocate/AllocatePod/GetTopologyHints/GetPodTopologyHints): `logger klog.Logger` → `ctx context.Context`.
- [ ] Update `policy_none.go`, `policy_static.go`, `policy_best_effort.go` impls.
- [ ] Update `fake_memory_manager.go`.
- [ ] Update all tests (`memory_manager_test.go`, `policy_static_test.go`) to pass `ctx`.
- [ ] Run `go build ./pkg/kubelet/cm/memorymanager/... && go test ./pkg/kubelet/cm/memorymanager/...`.

### Task 5: Thread `ctx` through Device Manager admission paths

Eliminate the `ctx := context.TODO()` at `pkg/kubelet/cm/devicemanager/manager.go:398`, and the `klog.TODO()` calls in `topology_hints.go:36, 91, 250`. The lower layers (`allocateContainerResources`, `devicesToAllocate`, endpoint RPCs) already accept `ctx` — this task connects them to the admission entry points.

**Files:**
- Modify: `pkg/kubelet/cm/devicemanager/types.go` (`Manager` interface: `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints` — add `ctx`)
- Modify: `pkg/kubelet/cm/devicemanager/manager.go` (`ManagerImpl.Allocate`, `AllocatePod` — accept `ctx`, drop `context.TODO()`)
- Modify: `pkg/kubelet/cm/devicemanager/topology_hints.go` (`ManagerImpl.GetTopologyHints`, `GetPodTopologyHints` — accept `ctx`; drop `klog.TODO()`; replace `klog.TODO().Info(…)` in `deviceNUMANodes` only if it is on the admission call path — leave alone otherwise since `deviceNUMANodes` is also reachable from non-admission code; if so, pass `ctx` to `deviceNUMANodes`)
- Modify: `pkg/kubelet/cm/devicemanager/manager.go` `GetDeviceRunContainerOptions:1016` — `m.Allocate(pod, container)` → `m.Allocate(ctx, pod, container)`
- Modify: `pkg/kubelet/cm/devicemanager/manager_test.go` — `MockEndpoint` already accepts ctx in `allocate`/`getPreferredAllocation`; update tests that call `testManager.Allocate(…)` etc.
- Modify: `pkg/kubelet/cm/devicemanager/topology_hints_test.go` — pass `ctx`
- Modify: `pkg/kubelet/cm/devicemanager/endpoint_test.go` — already uses ctx (no change needed, just verify)

- [ ] `types.go`: update `Manager` interface signatures.
- [ ] `manager.go`: update `Allocate` (drop the `// Use context.TODO()` comment block at lines 396–398), `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`. Each becomes `func (m *ManagerImpl) Allocate(ctx context.Context, …) error { logger := klog.FromContext(ctx); … }`.
- [ ] `topology_hints.go`: drop `klog.TODO()` in `GetTopologyHints` and `GetPodTopologyHints`; use `klog.FromContext(ctx)`.
- [ ] In `GetDeviceRunContainerOptions`, the inner `m.Allocate(pod, container)` re-allocation call must now pass `ctx`.
- [ ] Update `MockEndpoint` and all test call sites in `manager_test.go`, `topology_hints_test.go`.
- [ ] Verify nothing else creates a `context.TODO()` inside `pkg/kubelet/cm/devicemanager/`; if any remain on the admission path, replace them with the parameter. (Acceptable remainders: `Stop`, `UpdateAllocatedDevices`, `GetCapacity` — these are not on the admission path; leave their `klog.TODO()` / `logger klog.TODO()` alone for this PR.)
- [ ] Run `go build ./pkg/kubelet/cm/devicemanager/... && go test ./pkg/kubelet/cm/devicemanager/...`.

### Task 6: Wire `ctx` from TopologyManager `Scope.Admit` into HintProvider callers

After Tasks 2–5 land independently, the Scope still needs to actually call the new HintProvider methods with `ctx`. This task closes the loop. (If you fold this into Task 2, mark this task complete.)

**Files:**
- Modify: `pkg/kubelet/cm/topologymanager/scope.go` (`allocateAlignedResources` and `allocatePodAlignedResources` already take `ctx` from Task 2; ensure they pass it to `provider.Allocate(ctx, pod, container)` and `provider.AllocatePod(ctx, pod)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_container.go` (`accumulateProvidersHints` calls `provider.GetTopologyHints(ctx, pod, container)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_pod.go` (`accumulateProvidersHints` calls `provider.GetPodTopologyHints(ctx, pod)`)

- [ ] Verify every `provider.*` call in `pkg/kubelet/cm/topologymanager/scope*.go` passes `ctx`.
- [ ] Run `go build ./pkg/kubelet/cm/... && go test ./pkg/kubelet/cm/...`.

### Task 7: Thread `ctx` through Allocation Manager admission paths

The 2025 allocation manager owns `AddPod`, `RemovePod`, `handlePodResourcesResize`, and `canAdmitPod`. Each currently creates `klog.TODO()` or accepts `logger klog.Logger`. Per the task description, replace logger-only signatures with `ctx` on the admission path.

**Files:**
- Modify: `pkg/kubelet/allocation/allocation_manager.go`
  - `Manager` interface `AddPod(activePods []*v1.Pod, pod *v1.Pod) (bool, string, string)` → `AddPod(ctx context.Context, activePods []*v1.Pod, pod *v1.Pod)`
  - `manager.AddPod` impl: drop `klog.TODO()`, use `klog.FromContext(ctx)`
  - `manager.RemovePod(uid types.UID)` — only currently has `klog.TODO()`; remains a non-admission call (no `ctx` plumbed from any current caller). Leave as is unless a caller already has `ctx`; if so, accept `ctx context.Context`. Check `kubelet.go` callers — if they have `ctx`, add it.
  - `manager.handlePodResourcesResize(logger klog.Logger, pod *v1.Pod)` → `(ctx context.Context, pod *v1.Pod)`
  - `manager.canAdmitPod(logger, allocatedPods, pod, op)` → `canAdmitPod(ctx, allocatedPods, pod, op)`; inside loop call `podAdmitHandler.Admit(ctx, attrs)`
- Modify: `pkg/kubelet/allocation/handlers.go` — `podResizesAdmitHandler.Admit` is already updated in Task 1; either drop `h.logger` field and extract from `ctx`, or leave `h.logger` and ignore. Prefer dropping for cleanliness.
- Modify: `pkg/kubelet/kubelet.go:2882` — `kl.allocationManager.AddPod(kl.GetActivePods(), pod)` → `kl.allocationManager.AddPod(ctx, kl.GetActivePods(), pod)`
- Modify: every caller of `handlePodResourcesResize` inside `allocation_manager.go` (the resize loop at line 247) — pass `ctx` from `Run(ctx context.Context)` (line 195, already ctx-aware)
- Modify: `pkg/kubelet/allocation/*_test.go` — pass `ctx` from `ktesting.NewTestContext(t)`

- [ ] Update `Manager` interface `AddPod` signature; update impl to drop `klog.TODO()`.
- [ ] Update `handlePodResourcesResize` signature; thread `ctx` from the resize-handling caller inside `Run`.
- [ ] Update `canAdmitPod` signature; ensure it passes `ctx` to every `podAdmitHandler.Admit`.
- [ ] In `handlers.go`, simplify `podResizesAdmitHandler` to use `klog.FromContext(ctx)` instead of a stored `h.logger`; update the `NewPodResizesAdmitHandler` constructor to drop the `logger klog.Logger` parameter.
- [ ] Update `kubelet.go:1166` `NewPodResizesAdmitHandler(...)` call site to drop the logger argument.
- [ ] Update `kubelet.go:2882` to pass `ctx`.
- [ ] Update all `pkg/kubelet/allocation/*_test.go` test callers.
- [ ] Run `go build ./pkg/kubelet/... && go test ./pkg/kubelet/allocation/...`.

### Task 8: Add `ctx` to TopologyManager `Scope.RemoveContainer` and `manager.AddHintProvider` if reachable from admission

Smaller polish. `scope.RemoveContainer` (`pkg/kubelet/cm/topologymanager/scope.go:114`) currently does `ctx := context.TODO()`. This is reached from container teardown, **not** admission, so it falls outside the immediate scope of the task. **Defer this task** unless investigation in step 1 below shows otherwise.

**Files:**
- Inspect: `pkg/kubelet/cm/topologymanager/scope.go` `RemoveContainer`
- Inspect: `pkg/kubelet/cm/topologymanager/topology_manager.go` `AddHintProvider`

- [ ] Trace callers of `scope.RemoveContainer` and `Manager.RemoveContainer`. If every caller in the kubelet already has `ctx`, plumb it through and drop the `context.TODO()` here; otherwise leave alone (out of scope for this PR).
- [ ] Trace callers of `Manager.AddHintProvider(_ klog.Logger, h HintProvider)` (currently ignores its logger). If callers already have `ctx`, change to `AddHintProvider(ctx, h)`; otherwise leave alone.
- [ ] If any change is made, run `go build ./pkg/kubelet/cm/... && go test ./pkg/kubelet/cm/...`.

### Task 9: Verify the admission → device-plugin chain end-to-end and run integration tests

Confirm that the `ctx` plumbed in at `Kubelet.HandlePodAdditions` (or `Kubelet.SyncPod` for resize) now reaches `endpoint.allocate(ctx, devs)` without any intervening `context.TODO()` / `context.Background()` on the hot path. Spot-check by tracing the call graph manually and grepping for stragglers.

**Files:**
- Inspect (no edits expected): everything touched in Tasks 1–7.

- [ ] `grep -nE 'context\.(TODO|Background)\(\)' pkg/kubelet/cm/devicemanager/ pkg/kubelet/cm/topologymanager/ pkg/kubelet/cm/cpumanager/ pkg/kubelet/cm/memorymanager/ pkg/kubelet/lifecycle/ pkg/kubelet/eviction/ pkg/kubelet/sysctl/ pkg/kubelet/nodeshutdown/ pkg/kubelet/allocation/` — for each remaining hit, confirm it is **not** on the admission → device-plugin chain (e.g. it's in `Start`, reconcile loops, GC paths). Document each remaining occurrence in the PR description.
- [ ] `grep -nE 'klog\.TODO\(\)' …` (same packages) — same audit.
- [ ] Trace at least one path: `Kubelet.HandlePodAdditions` → `allocationManager.AddPod(ctx, …)` → `canAdmitPod(ctx, …)` → `predicateAdmitHandler.Admit(ctx, …)` → no longer happens via topology manager directly (predicate uses `pluginResourceUpdateFunc` for device manager resource update). Then trace `Kubelet.HandlePodAdditions` → `topologyManager.Admit(ctx, …)` (also called via `admitHandlers`) → `Scope.Admit(ctx, pod)` → `allocateAlignedResources(ctx, pod, container)` → `provider.Allocate(ctx, pod, container)` (DeviceManager) → `ManagerImpl.Allocate(ctx, pod, container)` → `allocateContainerResources(ctx, …)` → `endpoint.allocate(ctx, devs)`. Confirm every arrow has `ctx`.
- [ ] Run `go build ./...` from repo root.
- [ ] Run `go test ./pkg/kubelet/...`.
- [ ] Run `hack/verify-govet.sh` if it is fast on this tree (skip if slow); otherwise `go vet ./pkg/kubelet/...`.

### Task 10: Update release notes and PR description

The original PR carried `release-note: NONE` because no user-facing behavior changes — the context plumbed in today carries no deadline/cancellation signal yet. Keep the same posture; the value of this PR is enabling a future PR (per the upstream discussion linked at #127717) to attach a deadline to admission.

**Files:**
- Modify: PR description (not a file in the tree).

- [ ] Use `release-note: NONE`. Mention in the PR body that this is the follow-up to #128008, that it resolves the TODOs at `predicate.go:120`, `topology_manager.go:263`, and `devicemanager/manager.go:396`, and that it is a no-op at runtime because the propagated context currently carries no deadline.
- [ ] Link to the future intent (PR #127717) for adding a deadline on the admission context.

## Questions

1. **Scope of logger → ctx replacement.** The task description says "replace logger passed into the methods with the context instead of passing both whenever it will be needed." Some methods (`AddContainer`, `RemoveContainer`, `policyRemoveContainerByID`, `GetMemoryNUMANodes`, `markResourceUnhealthy`, `writeCheckpoint`, etc.) currently take `logger klog.Logger` and are reached from teardown / reconcile paths that do not currently have `ctx`. Two options:
   - **Option A (recommended)**: Scope this PR strictly to the admission → device-plugin chain. Leave `AddContainer`/`RemoveContainer`/reconcile signatures with their `logger klog.Logger` parameters untouched. A separate PR can refactor the rest.
   - **Option B**: Do everything in one PR — also plumb `ctx` through `AddContainer`, `RemoveContainer`, `Start`, `reconcileState`, `GetCapacity`, `UpdateAllocatedDevices`, etc. This roughly doubles the diff and is harder to review.
   - **Suggested: Option A** to keep the change reviewable and focused.

2. **`predicate.go` `getNodeAnyWayFunc`.** `predicateAdmitHandler.Admit` calls `w.getNodeAnyWayFunc(ctx, true)`. The `getNodeAnyWayFuncType` already accepts a `ctx`, so plumbing the parameter ctx into it instead of `context.TODO()` is trivial. Confirm.
   - **Suggested: yes, replace `context.TODO()` at predicate.go:122 with the parameter `ctx`** — same as Task 1 already plans.

3. **Should `Manager.RemovePod` in `allocation_manager.go` accept `ctx`?** Its callers (`kl.allocationManager.RemovePod(uid)`) are in pod-removal flows, some of which already have `ctx`. Adding `ctx` is cheap.
   - **Option A**: Add `ctx` since some callers have it. **(recommended)**
   - **Option B**: Leave as-is; out of scope.
   - **Suggested: Option A** — RemovePod sees `ctx` callers in the sync loop, and replacing the `klog.TODO()` is a one-line win.

4. **`TopologyManager.AddHintProvider(_ klog.Logger, h HintProvider)` already ignores its logger.** Should we drop the logger parameter entirely while we're touching neighboring code, or wait?
   - **Suggested: leave it for a follow-up.** Removing an exported parameter ripples to every caller in the kubelet wiring code without delivering value for this PR.
