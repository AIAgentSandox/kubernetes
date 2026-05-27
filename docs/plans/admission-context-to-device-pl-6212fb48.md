# Propagate `context.Context` through Pod Admission to Device Plugin

## Overview

Propagate `context.Context` from the kubelet's pod-admission entry points all the way down to the device-plugin gRPC calls in the Device Manager. This is the successor to abandoned PR [#128008](https://github.com/kubernetes/kubernetes/pull/128008): much of the upper-layer threading (Kubelet `HandlePodAdditions`, TopologyManager `Scope.Admit`, device-manager endpoint RPCs, `GetDeviceRunContainerOptions`, `allocateContainerResources`) has since been merged piecemeal, but the critical bridges in the middle still create `context.TODO()` / `klog.TODO()` locally. The remaining work is to plumb `ctx` through:

1. `lifecycle.PodAdmitHandler.Admit` (the interface that turns admission into a context-aware operation).
2. `cm/{cpumanager,memorymanager,devicemanager}` `Manager` methods used during admission (`Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`).
3. The `topologymanager.HintProvider` interface (`GetTopologyHints`, `GetPodTopologyHints`, `Allocate`, `AllocatePod`).
4. The `allocation.Manager` admission paths (`AddPod`, `handlePodResourcesResize`, `canAdmitPod`).

**Scope discipline:** this PR focuses strictly on the admission → device-plugin chain. Signatures, callers, and tests outside that chain (`AddContainer`, `RemoveContainer`, reconcile, `Start`, `GetCapacity`, GC, pod removal, etc.) are out of scope, even if they still hold `klog.TODO()` / `context.TODO()`. A follow-up PR can address them.

**Logger replacement rule (enforced on the admission chain only):** whenever a function on the admission → device-plugin chain previously took an explicit `klog.Logger` / `logr.Logger` parameter and is being given a `ctx`, drop the logger parameter and extract it via `logger := klog.FromContext(ctx)` inside the function. **Never pass both `ctx` and an explicit logger on an admission-path function.** This applies to the function signature, every caller, every test, and every fake/mock on the admission path. The only logger parameters that survive this PR are on methods reached **exclusively** from non-admission paths (e.g. `AddContainer`, `RemoveContainer`, reconcile, `Start`, `GetCapacity`, GC), which are out of scope. If a method is reached from both an admission path and a non-admission path, switch it to `ctx` and update the non-admission caller to pass a `ctx` (using its own `ctx` if available, or `context.TODO()` with a `// TODO: plumb ctx from caller` comment if not — but never both parameters).

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
- Use `klog.FromContext(ctx)` inside any function that previously had `logger := klog.TODO()` and is now given a `ctx`. **Never keep both an explicit logger parameter and a `ctx` parameter on the same admission-path function.** When in doubt, drop the logger and extract from `ctx`.
- Stage the change layer-by-layer (leaves last → roots first is impractical here because all signatures cascade); instead, change one interface plus its implementations and direct callers per task, leaving compilation green at the end of each task. Run `hack/update-codegen.sh` is **not** needed (no generated code changes).
- When updating an interface, also update mocks/fakes in the same task (e.g. `fake_topology_manager.go`, `fake_cpu_manager.go`, `fake_memory_manager.go`, `MockEndpoint` in `manager_test.go`).
- **Out-of-scope reminder**: do not touch `RemovePod`, `AddContainer`, `RemoveContainer`, reconcile loops, `Start`, `GetCapacity`, GC, `AddHintProvider`, or anything else off the admission → device-plugin chain in this PR. Existing `klog.TODO()` / `context.TODO()` in those code paths stays.

## Implementation Steps

### Task 1: Add `ctx context.Context` to `PodAdmitHandler.Admit`

Promote `ctx` to a first-class parameter on the interface that gates all kubelet pod admission. This is the keystone change — every admission handler signature flows from here. Resolve the TODO at `pkg/kubelet/lifecycle/predicate.go:120–123` and `pkg/kubelet/cm/topologymanager/topology_manager.go:263–266` in the same task. Also replace `context.TODO()` at `predicate.go:122` with the parameter `ctx` so `w.getNodeAnyWayFunc(ctx, true)` flows real context.

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
- Modify: `pkg/kubelet/allocation/handlers.go` (`podResizesAdmitHandler.Admit` — drop the `h.logger` field; the constructor `NewPodResizesAdmitHandler` loses its `logger klog.Logger` parameter; inside `Admit`, use `klog.FromContext(ctx)`)
- Modify: `pkg/kubelet/allocation/allocation_manager.go` (`canAdmitPod` — pass `ctx` into `podAdmitHandler.Admit(ctx, attrs)`; see Task 7)
- Modify: tests for every file above. Each test already calls `Admit(attrs)`; switch to `Admit(ctx, attrs)` using `_, ctx := ktesting.NewTestContext(t)` (the import is already used widely in these packages).

- [x] Update `PodAdmitHandler.Admit` interface signature in `pkg/kubelet/lifecycle/interfaces.go` to `Admit(ctx context.Context, attrs *PodAdmitAttributes) PodAdmitResult` and add `import "context"`.
- [x] Update every concrete implementation (8+ types listed above) to accept `ctx` and pass it through. In `predicate.go` replace `ctx := context.TODO()` at line 122 with the parameter — the existing `w.getNodeAnyWayFunc(ctx, true)` call already takes a ctx, so this is a parameter swap. In `topology_manager.go` `Admit` replace the inline `context.TODO()` and remove the TODO comment at lines 263–264.
- [x] In `pkg/kubelet/allocation/handlers.go`, drop the `logger klog.Logger` field on `podResizesAdmitHandler` and the `logger` parameter on `NewPodResizesAdmitHandler`. Inside `Admit`, extract `logger := klog.FromContext(ctx)` if logging is performed.
- [x] Update `kubelet.go` `NewPodResizesAdmitHandler(...)` call site (around `kubelet.go:1166`) to drop the logger argument.
- [x] Update `allocation.manager.canAdmitPod` to accept `ctx` (see Task 7) and pass it into every `podAdmitHandler.Admit(ctx, attrs)` call in the loop.
- [x] Update every test that calls `.Admit(attrs)` on a `PodAdmitHandler` to pass `ctx` from `ktesting.NewTestContext(t)`. Files include `eviction_manager_test.go`, `nodeshutdown_manager_linux_test.go`, `allowlist_test.go`, `fake_topology_manager_test.go`, `kubelet_test.go` (`testPodAdmitHandler.Admit`), and `allocation/*_test.go`.
- [x] Run `go build ./pkg/kubelet/... && go test ./pkg/kubelet/lifecycle/... ./pkg/kubelet/eviction/... ./pkg/kubelet/sysctl/... ./pkg/kubelet/nodeshutdown/... ./pkg/kubelet/cm/topologymanager/... ./pkg/kubelet/allocation/...`.

### Task 2: Thread `ctx` through `topologymanager.HintProvider`

The HintProvider interface is the contract between TopologyManager and the CPU/Memory/Device managers. Adding `ctx` here unlocks ctx-aware calls inside `Scope.Admit` (which already has `ctx`) and lets the underlying managers stop fabricating `klog.TODO()`. Per the logger-replacement rule, **drop any pre-existing `klog.Logger` parameter from helpers in `scope_container.go` and `scope_pod.go`** as `ctx` is added; extract `logger := klog.FromContext(ctx)` once at the top of each function that needs it.

**Files:**
- Modify: `pkg/kubelet/cm/topologymanager/topology_manager.go` (`HintProvider` interface, lines 106–124)
- Modify: `pkg/kubelet/cm/topologymanager/scope.go` (`allocateAlignedResources`, `allocatePodAlignedResources` — accept `ctx` from `Scope.Admit`; pass to `provider.Allocate` / `provider.AllocatePod`; pass to `admitPolicyNone` which currently has no ctx)
- Modify: `pkg/kubelet/cm/topologymanager/scope_container.go` (`accumulateProvidersHints`, `calculateAffinity` — accept `ctx`; **drop any existing `logger klog.Logger` parameter**; extract logger via `klog.FromContext(ctx)` inside if needed; `provider.GetTopologyHints(ctx, …)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_pod.go` (`accumulateProvidersHints`, `calculateAffinity`, `admitUsingContainerResources`, `admitUsingPodResources` — accept `ctx`; **drop any existing `logger klog.Logger` parameter**; extract via `klog.FromContext(ctx)` inside; `provider.GetPodTopologyHints(ctx, …)`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_none.go` (`noneScope.Admit` already takes `ctx`; just forward it)
- Modify: `pkg/kubelet/cm/topologymanager/topology_manager_test.go` (`mockHintProvider` — update signatures; update `TestAdmit` to pass `ctx`)
- Modify: `pkg/kubelet/cm/topologymanager/scope_container_test.go` (`calculateAffinity` calls)
- Modify: `pkg/kubelet/cm/topologymanager/scope_pod_test.go` (`calculateAffinity`, `accumulateProvidersHints` calls)
- Modify: `pkg/kubelet/cm/topologymanager/policy_test.go` (`provider.GetTopologyHints` calls in `testPolicyMerge`)

- [x] Change `HintProvider` interface (`GetTopologyHints`, `GetPodTopologyHints`, `Allocate`, `AllocatePod`) to take `ctx context.Context` as the first parameter.
- [x] Update `scope.allocateAlignedResources` to take `ctx`; update `scope.allocatePodAlignedResources` to take `ctx`; update `scope.admitPolicyNone` to take `ctx`; thread `ctx` from `Scope.Admit` through these helpers.
- [x] Update `containerScope.accumulateProvidersHints` and `calculateAffinity` to take `ctx`; **delete the existing `logger klog.Logger` parameter** (if present); inside, extract `logger := klog.FromContext(ctx)` once.
- [x] Update `podScope.accumulateProvidersHints` and `calculateAffinity` similarly — drop any `klog.Logger` parameter; extract from `ctx` inside.
- [x] Update `mockHintProvider` and all tests under `pkg/kubelet/cm/topologymanager/` to pass `ctx` from `ktesting.NewTestContext(t)`.
- [x] To keep the tree compiling at the end of this task, also perform the **interface-signature-only** edits on the CPU/Memory/Device managers' `Allocate`/`AllocatePod`/`GetTopologyHints`/`GetPodTopologyHints` (just add `ctx context.Context` as the first parameter and use `_ = ctx` inside if needed). The full internal cleanup of `klog.TODO()` and the logger-parameter removal inside those managers is done in Tasks 3–5.
- [x] Run `go build ./pkg/kubelet/cm/... && go test ./pkg/kubelet/cm/topologymanager/...`.

### Task 3: Thread `ctx` through CPU Manager admission paths

Eliminate the `klog.TODO()` lines at `pkg/kubelet/cm/cpumanager/cpu_manager.go:270, 289, 358, 366`. The CPU policies currently take `logger logr.Logger` on their admission methods; per the logger-replacement rule, **delete the `logger logr.Logger` parameter** when `ctx` is added and extract it via `klog.FromContext(ctx)` inside the implementation.

**Files:**
- Modify: `pkg/kubelet/cm/cpumanager/cpu_manager.go` (`Manager` interface methods `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints` — add `ctx`; `manager.*` implementations drop their `klog.TODO()`)
- Modify: `pkg/kubelet/cm/cpumanager/policy.go` (`Policy` interface — `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`: **replace `logger logr.Logger` with `ctx context.Context`**)
- Modify: `pkg/kubelet/cm/cpumanager/policy_none.go`, `policy_static.go` — update signatures; **delete the `logger logr.Logger` parameter**; extract `logger := klog.FromContext(ctx)` at top of each method that needs logging
- Modify: `pkg/kubelet/cm/cpumanager/fake_cpu_manager.go` — update fake to match (no logger parameter on admission methods)
- Modify: `pkg/kubelet/cm/cpumanager/cpu_manager_test.go` (`mockPolicy` + all `Allocate`/hint test calls) — drop any logger argument; pass `ctx`
- Modify: `pkg/kubelet/cm/cpumanager/policy_none_test.go`, `policy_static_test.go`, `topology_hints_test.go` — pass `ctx`; drop logger argument
- Leave alone (do not touch in this task): `AddContainer`, `RemoveContainer`, `policyRemoveContainer*`, `Start`, `reconcileState` — these are not on the admission path; they keep their existing `logger logr.Logger` / `ctx` signatures.

- [x] `cpu_manager.go` `Manager` interface: `Allocate(ctx, pod, container)`, `AllocatePod(ctx, pod)`, `GetTopologyHints(ctx, pod, container)`, `GetPodTopologyHints(ctx, pod)`. **No logger parameter on any of these.**
- [x] `cpu_manager.go` `manager.*` impls: drop `klog.TODO()`, use `klog.FromContext(ctx)`, pass `ctx` into `m.policy.*` calls (without a logger argument).
- [x] `policy.go` `Policy` interface: **replace `logger logr.Logger` with `ctx context.Context`** on `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints` (keep `state.State`, `*v1.Pod`, `*v1.Container` arguments).
- [x] `policy_none.go`, `policy_static.go`: update impls — delete the `logger logr.Logger` parameter; extract logger via `klog.FromContext(ctx)` inside.
- [x] `fake_cpu_manager.go`: update fake signatures (no logger parameter on admission methods).
- [x] `cpu_manager_test.go`, `policy_none_test.go`, `policy_static_test.go`, `topology_hints_test.go`: use `_, ctx := ktesting.NewTestContext(t)` and pass `ctx` (drop any explicit logger argument).
- [x] Run `go build ./pkg/kubelet/cm/cpumanager/... && go test ./pkg/kubelet/cm/cpumanager/...`.

### Task 4: Thread `ctx` through Memory Manager admission paths

Mirror Task 3 for the memory manager. Eliminate `klog.TODO()` at `pkg/kubelet/cm/memorymanager/memory_manager.go:268, 283`. Memory policies currently take `klog.Logger` on their admission methods — **delete it** and extract via `klog.FromContext(ctx)` instead.

**Files:**
- Modify: `pkg/kubelet/cm/memorymanager/memory_manager.go` (`Manager` interface `Allocate`, `AllocatePod`, `GetTopologyHints`, `GetPodTopologyHints`)
- Modify: `pkg/kubelet/cm/memorymanager/policy.go` (`Policy` interface — **replace `klog.Logger` parameter with `ctx`**; do not keep both)
- Modify: `pkg/kubelet/cm/memorymanager/policy_none.go`, `policy_static.go`, `policy_best_effort.go` — update signatures; drop the `klog.Logger` parameter; extract from `ctx`
- Modify: `pkg/kubelet/cm/memorymanager/fake_memory_manager.go`
- Modify: `pkg/kubelet/cm/memorymanager/memory_manager_test.go` (`mockPolicy`, `TestAddContainer`, `TestGetTopologyHints`, `TestAllocateAndAddPodWithInitContainers`, etc.)
- Modify: `pkg/kubelet/cm/memorymanager/policy_static_test.go`
- Leave alone: `GetMemoryNUMANodes`, `RemoveContainer`, `AddContainer` (non-admission, keep `klog.Logger` param)

- [x] Update `Manager` interface and `manager.*` impls — admission methods take `ctx` only (no logger).
- [x] Update `Policy` interface (Allocate/AllocatePod/GetTopologyHints/GetPodTopologyHints): **`logger klog.Logger` → `ctx context.Context`** (replace, not append).
- [x] Update `policy_none.go`, `policy_static.go`, `policy_best_effort.go` impls — drop the logger parameter; extract via `klog.FromContext(ctx)` inside.
- [x] Update `fake_memory_manager.go`.
- [x] Update all tests (`memory_manager_test.go`, `policy_static_test.go`) to pass `ctx` and remove any logger argument.
- [x] Run `go build ./pkg/kubelet/cm/memorymanager/... && go test ./pkg/kubelet/cm/memorymanager/...`.

### Task 5: Thread `ctx` through Device Manager admission paths

Eliminate the `ctx := context.TODO()` at `pkg/kubelet/cm/devicemanager/manager.go:398`, and the `klog.TODO()` calls in `topology_hints.go:36, 91, 250`. The lower layers (`allocateContainerResources`, `devicesToAllocate`, endpoint RPCs) already accept `ctx` — this task connects them to the admission entry points. Device-manager admission methods do not currently have an explicit logger parameter (they construct `klog.TODO()` internally), so the rule "don't pass both" reduces here to "use `klog.FromContext(ctx)` and delete the local `klog.TODO()`".

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

- [ ] Verify every `provider.*` call in `pkg/kubelet/cm/topologymanager/scope*.go` passes `ctx` and **no** explicit logger argument.
- [ ] Run `go build ./pkg/kubelet/cm/... && go test ./pkg/kubelet/cm/...`.

### Task 7: Thread `ctx` through Allocation Manager admission paths

The 2025 allocation manager owns `AddPod`, `handlePodResourcesResize`, and `canAdmitPod` on the admission path. Each currently creates `klog.TODO()` or accepts `logger klog.Logger`. Per the logger-replacement rule, **replace logger-only signatures with `ctx`** on these admission-path methods — never pass both. **`RemovePod` is intentionally out of scope** for this PR (pod-removal flow, not admission); leave its signature and `klog.TODO()` untouched.

**Files:**
- Modify: `pkg/kubelet/allocation/allocation_manager.go`
  - `Manager` interface `AddPod(activePods []*v1.Pod, pod *v1.Pod) (bool, string, string)` → `AddPod(ctx context.Context, activePods []*v1.Pod, pod *v1.Pod)`
  - `manager.AddPod` impl: drop `klog.TODO()`, use `klog.FromContext(ctx)`
  - `manager.handlePodResourcesResize(logger klog.Logger, pod *v1.Pod)` → `handlePodResourcesResize(ctx context.Context, pod *v1.Pod)` — **delete the `logger klog.Logger` parameter**; extract via `klog.FromContext(ctx)` inside
  - `manager.canAdmitPod(logger, allocatedPods, pod, op)` → `canAdmitPod(ctx, allocatedPods, pod, op)` — **delete the `logger` parameter**; inside loop call `podAdmitHandler.Admit(ctx, attrs)`
  - **Do NOT touch `Manager.RemovePod` or `manager.RemovePod`.** Its `klog.TODO()` stays for a follow-up PR.
- Modify: `pkg/kubelet/allocation/handlers.go` — `podResizesAdmitHandler.Admit` is updated in Task 1: the `h.logger` field is dropped; `NewPodResizesAdmitHandler` loses its logger parameter. Confirm this is done.
- Modify: `pkg/kubelet/kubelet.go:2882` — `kl.allocationManager.AddPod(kl.GetActivePods(), pod)` → `kl.allocationManager.AddPod(ctx, kl.GetActivePods(), pod)`
- Modify: every caller of `handlePodResourcesResize` inside `allocation_manager.go` (the resize loop at line 247) — pass `ctx` from `Run(ctx context.Context)` (line 195, already ctx-aware); do not pass a logger argument
- Modify: every caller of `canAdmitPod` — pass `ctx` (not a logger)
- Modify: `pkg/kubelet/allocation/*_test.go` — pass `ctx` from `ktesting.NewTestContext(t)`; drop any logger argument

- [ ] Update `Manager` interface `AddPod` signature; update impl to drop `klog.TODO()`.
- [ ] Update `handlePodResourcesResize` signature — **drop the `logger` parameter, add `ctx`**; thread `ctx` from the resize-handling caller inside `Run`.
- [ ] Update `canAdmitPod` signature — **drop the `logger` parameter, add `ctx`**; ensure it passes `ctx` to every `podAdmitHandler.Admit`.
- [ ] In `handlers.go`, confirm `podResizesAdmitHandler` no longer stores `h.logger`; `NewPodResizesAdmitHandler` constructor signature lost its `logger klog.Logger` parameter (work begun in Task 1).
- [ ] Update `kubelet.go:1166` `NewPodResizesAdmitHandler(...)` call site to drop the logger argument (work begun in Task 1; verify).
- [ ] Update `kubelet.go:2882` to pass `ctx`.
- [ ] **Verify `Manager.RemovePod` / `manager.RemovePod` are not modified** — they remain off the admission path and out of scope for this PR.
- [ ] Update all `pkg/kubelet/allocation/*_test.go` test callers — pass `ctx`, drop any logger argument.
- [ ] Run `go build ./pkg/kubelet/... && go test ./pkg/kubelet/allocation/...`.

### Task 8: Audit remaining mixed `ctx` + `logger` signatures on the admission chain

Final sweep specifically targeting the logger-replacement rule. Across all packages touched in Tasks 1–7, grep for any function on the admission → device-plugin chain that ends up with both a `ctx context.Context` parameter and a `klog.Logger` / `logr.Logger` parameter. If found, drop the logger and extract via `klog.FromContext(ctx)`.

**Files:**
- Inspect: every file modified in Tasks 1–7.

- [ ] `grep -nE 'ctx context\.Context.*klog\.Logger|klog\.Logger.*ctx context\.Context|ctx context\.Context.*logr\.Logger|logr\.Logger.*ctx context\.Context' pkg/kubelet/lifecycle pkg/kubelet/eviction pkg/kubelet/sysctl pkg/kubelet/nodeshutdown pkg/kubelet/cm/topologymanager pkg/kubelet/cm/cpumanager pkg/kubelet/cm/memorymanager pkg/kubelet/cm/devicemanager pkg/kubelet/allocation`.
- [ ] For each hit on the admission → device-plugin chain, drop the logger parameter, update every caller, extract via `klog.FromContext(ctx)` inside.
- [ ] For each hit reachable **only** from non-admission paths (e.g. `Start`, `reconcileState`, `AddContainer`, `RemoveContainer`, `RemovePod`, `AddHintProvider`), leave it alone — explicitly out of scope for this PR.
- [ ] Run `go build ./pkg/kubelet/... && go test ./pkg/kubelet/...`.

### Task 9: Verify the admission → device-plugin chain end-to-end and run integration tests

Confirm that the `ctx` plumbed in at `Kubelet.HandlePodAdditions` (or `Kubelet.SyncPod` for resize) now reaches `endpoint.allocate(ctx, devs)` without any intervening `context.TODO()` / `context.Background()` on the hot path. Spot-check by tracing the call graph manually and grepping for stragglers.

**Files:**
- Inspect (no edits expected): everything touched in Tasks 1–8.

- [ ] `grep -nE 'context\.(TODO|Background)\(\)' pkg/kubelet/cm/devicemanager/ pkg/kubelet/cm/topologymanager/ pkg/kubelet/cm/cpumanager/ pkg/kubelet/cm/memorymanager/ pkg/kubelet/lifecycle/ pkg/kubelet/eviction/ pkg/kubelet/sysctl/ pkg/kubelet/nodeshutdown/ pkg/kubelet/allocation/` — for each remaining hit, confirm it is **not** on the admission → device-plugin chain (e.g. it's in `Start`, reconcile loops, GC paths, `RemovePod`, `RemoveContainer`). Acceptable remainders are explicitly out of scope per the scope-discipline rule in the Overview.
- [ ] `grep -nE 'klog\.TODO\(\)' …` (same packages) — same audit; same acceptable-remainder rule.
- [ ] Trace at least one path: `Kubelet.HandlePodAdditions` → `allocationManager.AddPod(ctx, …)` → `canAdmitPod(ctx, …)` → `predicateAdmitHandler.Admit(ctx, …)` → no longer happens via topology manager directly (predicate uses `pluginResourceUpdateFunc` for device manager resource update). Then trace `Kubelet.HandlePodAdditions` → `topologyManager.Admit(ctx, …)` (also called via `admitHandlers`) → `Scope.Admit(ctx, pod)` → `allocateAlignedResources(ctx, pod, container)` → `provider.Allocate(ctx, pod, container)` (DeviceManager) → `ManagerImpl.Allocate(ctx, pod, container)` → `allocateContainerResources(ctx, …)` → `endpoint.allocate(ctx, devs)`. Confirm every arrow has `ctx` and **no** explicit logger argument.
- [ ] Run `go build ./...` from repo root.
- [ ] Run `go test ./pkg/kubelet/...`.
- [ ] Run `hack/verify-govet.sh` if it is fast on this tree (skip if slow); otherwise `go vet ./pkg/kubelet/...`.
