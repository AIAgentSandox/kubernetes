# Resize After OOM Kill — e2e_node Tests

## Overview

Add two new `test/e2e_node` (single-host) tests for the In-Place Pod Vertical Scaling (IPPVS) feature, exercising the kubelet's resize actuation path directly without the scheduler / quota / limit-ranger components that the existing `test/e2e/node/pod_resize.go` requires:

1. **Successful resize** — happy path: create a guaranteed pod, patch via the `resize` subresource, verify cgroup memory limits, allocated resources, status, and resize-status conditions are updated.
2. **Resize after OOMKill** — reproduce the bug-report scenario: pod is OOMKilled because its memory limit is too small, patch via the `resize` subresource while in OOMKilled state, then verify the pod recovers and the *pod-level cgroup* `memory.max` reflects the new (larger) value. If verification of pod-level cgroup memory currently fails (the bug under investigation), leave the failing assertion in place behind a `ginkgo.Skip` with an inline comment that references the bug.

Each test is implemented and validated as a separate task. The validation runs `make test-e2e-node` with a precise `FOCUS=` and the required `--kubelet-flags="--fail-swap-on=false"`.

## Context

- **Files involved:**
  - Create: `test/e2e_node/pod_resize_test.go` — new single-host resize tests in package `e2enode`.
- **Existing patterns to follow:**
  - `test/e2e/node/pod_resize.go` and `test/e2e/common/node/pod_resize.go` — full multi-component resize tests (model the patch / verify / wait helpers from these).
  - Helper package `test/e2e/common/node/framework/podresize` (`MakePodWithResizableContainers`, `MakeResizePatch`, `VerifyPodResources`, `VerifyPodStatusResources`, `VerifyPodContainersCgroupValues`, `VerifyPodCgroupValues`, `WaitForPodResizeActuation`, `ExpectPodResized`, `UpdateExpectedContainerRestarts`).
  - Helper package `test/e2e/common/node/framework/cgroups` (`ContainerResources`, `VerifyPodCgroups`, `ConfigureHostPathForPodCgroup`, `IsPodOnCgroupv2Node`, `VerifyCgroupValue`, `cgroupv2MemLimitFile = "memory.max"`).
  - `test/e2e_node/framework.go` — `SIGDescribe = framework.SIGDescribe("node")` (use this, do NOT redefine).
  - `test/e2e_node/oomkiller_linux_test.go` — pattern for triggering an OOMKill (busybox + `dd if=/dev/zero of=/dev/null bs=20M` with a tight memory limit) and for `tempSetCurrentKubeletConfig` if kubelet config tweaks are needed.
  - `test/e2e_node/container_restart_test.go` — pattern for `tempSetCurrentKubeletConfig` + per-context Ginkgo setup.
  - `test/e2e_node/image_volume.go` and `kubelet_server_tls_test.go` — top-of-file comment showing the `make test-e2e-node` invocation with `--kubelet-flags="--fail-swap-on=false"`.
- **Feature gate:** `InPlacePodVerticalScaling` is GA + locked at `1.35` (see `pkg/features/kube_features.go:1471`), so no `framework.WithFeatureGate(...)` decorator is required for the basic resize. Initial container resizing (`InPlacePodVerticalScalingInitContainers`) is **not** exercised here. Pod-level resources (`InPlacePodLevelResourcesVerticalScaling`) is **not** exercised here; tests use container-level requests/limits which is sufficient for the OOM-kill scenario.
- **Why e2e_node and not e2e/node:** The note at `test/e2e/common/node/pod_resize.go:857-863` explains that scheduler/quota/limit-ranger tests live in `test/e2e/node/pod_resize.go` because the node-only test environment doesn't run those components. The two tests planned here only exercise kubelet resize actuation, which is exactly what `test/e2e_node` is for.
- **Dependencies:** None new. Reuses `podresize` and `cgroups` packages, `busybox` image (`busyboxImage` constant already in package), `e2epod.NewPodClient`, ginkgo/gomega.

## Development Approach

- **Testing approach:** Follow `test/e2e_node` conventions — package `e2enode`, `SIGDescribe(...)` with `f.WithSerial()` and `feature.InPlacePodVerticalScaling`, `framework.NewDefaultFramework("...")` per `Describe`, `e2epod.NewPodClient(f)` for pod lifecycle, `ginkgo.By(...)` for step narration, `framework.ExpectNoError` for fatal errors, and explicit `podClient.DeleteSync` cleanup. Re-use `podresize` and `cgroups` helpers — do not duplicate.
- **Linux only:** Both tests use cgroup paths and OOM behavior that are Linux-specific. Mark the file `//go:build linux` (matching `oomkiller_linux_test.go` / `container_restart_test.go`).
- Complete each task fully (test written, code compiles, `make test-e2e-node FOCUS=...` runs the new test and it passes — or, for Task 2, fails specifically at the documented pod-cgroup assertion at which point it is gated behind `ginkgo.Skip`) before moving to the next.
- **CRITICAL: all tests must pass before starting next task.** Task 1 must pass cleanly. Task 2 must either pass cleanly or, if it hits the bug, be left in a state where the resize-and-recover portion passes and the pod-cgroup `memory.max` check is wrapped in `ginkgo.Skip("<bug reference>")` so the suite is green.

## Implementation Steps

### Task 1: Successful in-place resize (happy path)

Create a single-host kubelet test that exercises the kubelet resize actuation end-to-end on a guaranteed-QoS pod with a single container, increasing both CPU and memory limits, and verifying:

- the pod spec resources are updated post-patch,
- `Status.AllocatedResources` and `Status.ContainerStatuses[*].Resources` reflect the new values,
- container cgroup limits (CPU + memory) match the new values on the node,
- the `PodResizeInProgress` / `PodResizePending` conditions are cleared (i.e. `WaitForPodResizeActuation` returns cleanly),
- the pod stays `Ready`.

**Files:**
- Create: `test/e2e_node/pod_resize_test.go`

Skeleton (illustrative; final code will follow existing helpers):

```go
//go:build linux

package e2enode

import (
    "context"
    "strconv"
    "time"

    v1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/types"
    "k8s.io/kubernetes/test/e2e/common/node/framework/cgroups"
    "k8s.io/kubernetes/test/e2e/common/node/framework/podresize"
    "k8s.io/kubernetes/test/e2e/feature"
    "k8s.io/kubernetes/test/e2e/framework"
    e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
    admissionapi "k8s.io/pod-security-admission/api"

    "github.com/onsi/ginkgo/v2"
)

// Run locally:
//   make test-e2e-node FOCUS="Pod InPlace Resize \(node\)" \
//     TEST_ARGS='--kubelet-flags="--fail-swap-on=false"'
var _ = SIGDescribe("Pod InPlace Resize (node)", framework.WithSerial(), feature.InPlacePodVerticalScaling, func() {
    f := framework.NewDefaultFramework("pod-resize-node-tests")
    f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

    ginkgo.It("should resize CPU and memory of a guaranteed pod in place", func(ctx context.Context) {
        // 1. Build originalContainers (guaranteed, 100m/100m CPU, 64Mi/64Mi mem)
        // 2. Build expectedContainers (200m/200m CPU, 128Mi/128Mi mem)
        // 3. Create pod via podresize.MakePodWithResizableContainers
        // 4. podClient.CreateSync, then VerifyPodResources / VerifyPodStatusResources / VerifyPodContainersCgroupValues
        // 5. patch := podresize.MakeResizePatch(...); Patch with subresource "resize"
        // 6. expected := podresize.UpdateExpectedContainerRestarts(...)
        // 7. resizedPod := podresize.WaitForPodResizeActuation(...)
        // 8. podresize.ExpectPodResized(ctx, f, resizedPod, expected)
        // 9. VerifyPodResources(resizedPod, expected, nil) and VerifyPodContainersCgroupValues(...)
        // 10. podClient.DeleteSync
    })
})
```

- [ ] Add `//go:build linux` and copyright header (match existing files such as `container_restart_test.go`).
- [ ] Add file-top comment block showing the exact `make test-e2e-node` command including `--kubelet-flags="--fail-swap-on=false"` and the FOCUS regex.
- [ ] Define `SIGDescribe("Pod InPlace Resize (node)", framework.WithSerial(), feature.InPlacePodVerticalScaling, ...)` (no FeatureGate decorator needed — GA-locked).
- [ ] Implement `should resize CPU and memory of a guaranteed pod in place` using `podresize.MakePodWithResizableContainers`, `podresize.MakeResizePatch`, `podresize.WaitForPodResizeActuation`, `podresize.ExpectPodResized`, `podresize.VerifyPodResources`, `podresize.VerifyPodStatusResources`, `podresize.VerifyPodContainersCgroupValues`. Use `e2epod.MustMixinRestrictedPodSecurity` and `e2epod.NewPodClient(f).CreateSync`.
- [ ] Choose values that avoid host-resource flakiness: original `cpu: 100m/100m, memory: 64Mi/64Mi` → expected `cpu: 200m/200m, memory: 128Mi/128Mi`. Use `cgroups.ContainerResources` to express them.
- [ ] Skip on non-Linux / non-cgroupv2 hosts up front if needed (mirror `IsPodOnCgroupv2Node` usage in `VerifyPodContainersCgroupValues` — no extra guard needed for `test/e2e_node` since the build tag covers OS).
- [ ] Add explicit `podClient.DeleteSync` in the test body (don't rely on namespace teardown) to match the helper pattern in `test/e2e/common/node/pod_resize.go`.
- [ ] Build: `make WHAT=test/e2e_node/e2e_node.test` (or `go build ./test/e2e_node/...`) compiles clean.
- [ ] Validate locally with:
      ```
      make test-e2e-node FOCUS="Pod InPlace Resize \(node\).*should resize CPU and memory" \
        TEST_ARGS='--kubelet-flags="--fail-swap-on=false"'
      ```
      Test must pass.
- [ ] Write/update tests (this task **is** the test addition; no separate test code needed).

### Task 2: Resize after OOMKill

Add a second `It` block in the same `SIGDescribe` that:

1. Creates a guaranteed pod whose memory limit (e.g. `64Mi`) is small enough that a `dd if=/dev/zero of=/dev/null bs=200M` workload OOMKills the container.
2. Waits until the container status reports `LastTerminationState.Terminated.Reason == "OOMKilled"` (or `State.Terminated.Reason == "OOMKilled"` if it has not been restarted yet — pod restart policy is `OnFailure`/`Never`; choose `Never` so the pod stays in OOMKilled and is not restarted, mirroring the bug-report scenario where the patch happens *while* the pod is OOMKilled).
3. Patches the pod via the `resize` subresource to raise both `requests.memory` and `limits.memory` (e.g. to `256Mi`).
4. Asserts that:
   - the resize is accepted by the API server,
   - the pod's `Status.ContainerStatuses[*].Resources` and `Status.AllocatedResources` reflect the new values,
   - container-level cgroup `memory.max` reflects the new value (via `podresize.VerifyPodContainersCgroupValues`),
   - **pod-level cgroup `memory.max` reflects the new value** — this is the assertion the bug report says is broken. Wrap *only* this assertion in a `ginkgo.Skip` if it fails, with a comment pointing at the bug.
5. If the kubelet eventually restarts the container with the new limits, also verify the workload no longer OOMKills (Consistently observe `Status.Phase != Failed` for a short window). This part is optional bonus; keep the focused assertion strictly on resize-after-OOMKill correctness.

The bug snippet shows the symptom: after the resize patch, `memory.max` at the pod-cgroup level is still the **original** value (e.g. 128Mi) instead of the requested 256Mi. Use `podresize.VerifyPodCgroupValues` (which checks `memory.max` on the pod cgroup via `cgroups.VerifyPodCgroups`). To reach the pod-cgroup file at all, the test pod must mount the host cgroup path — `cgroups.ConfigureHostPathForPodCgroup(testPod)` does this; call it before `podClient.CreateSync`. This matches the `mountPodCgroup` parameter used in `test/e2e/common/node/pod_resize.go:931`.

**Files:**
- Modify: `test/e2e_node/pod_resize_test.go`

- [ ] Add `should accept resize after the container is OOMKilled and update pod cgroup memory limit` as an `It` inside the same `SIGDescribe` from Task 1.
- [ ] Construct a guaranteed pod with `RestartPolicy: v1.RestartPolicyNever`, one container running a workload guaranteed to OOMKill at the chosen limit. Use the busybox `dd`-based command pattern from `oomkiller_linux_test.go:243-272` (`"sleep 5 && dd if=/dev/zero of=/dev/null bs=200M"`). Set `requests.memory == limits.memory == 64Mi`, same for CPU at `100m`.
- [ ] Call `cgroups.ConfigureHostPathForPodCgroup(testPod)` so pod-level cgroup files are reachable for verification.
- [ ] Wait for the container status `Terminated.Reason == "OOMKilled"` (and `ExitCode == 137`) using `framework.Gomega().Eventually(...)` or by polling `f.ClientSet.CoreV1().Pods(...).Get`. Reuse the pattern in `oomkiller_linux_test.go:185-203` (`verifyReasonForOOMKilledContainer`) — copy the helper into this file rather than refactoring the existing one to keep blast radius small. Note the existing helper tolerates runtimes that don't surface the `OOMKilled` reason consistently (containerd issue #8893); replicate that tolerance.
- [ ] Patch via `resize` subresource using `podresize.MakeResizePatch(original, resized, nil, nil)` and `f.ClientSet.CoreV1().Pods(ns).Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")`.
- [ ] Wait for `podresize.WaitForPodResizeActuation` to return — this also confirms the `PodResizeInProgress` / `PodResizePending` conditions clear and `observedGeneration` catches up.
- [ ] Verify pod-spec resources and per-container cgroup values: `podresize.VerifyPodResources(resizedPod, expected, nil)` and `podresize.VerifyPodContainersCgroupValues(ctx, f, resizedPod, expected)`.
- [ ] Verify pod-level cgroup `memory.max`: call `podresize.VerifyPodCgroupValues(ctx, f, resizedPod)`. If this assertion fails because of the documented bug, wrap **only this single call** with the pattern:
      ```go
      // TODO(<bug-link-or-issue>): pod-level cgroup memory.max is not updated after a
      // resize that follows an OOMKill. Re-enable once fixed.
      if err := podresize.VerifyPodCgroupValues(ctx, f, resizedPod); err != nil {
          e2eskipper.Skipf("known issue: pod-level cgroup memory.max not updated post-OOMKill resize: %v", err)
      }
      ```
      and import `e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"`. Do **not** also skip the earlier per-container cgroup check — that one is expected to pass.
- [ ] `podClient.DeleteSync` at the end.
- [ ] Build: `go build ./test/e2e_node/...` clean.
- [ ] Validate locally with:
      ```
      make test-e2e-node FOCUS="Pod InPlace Resize \(node\).*resize after the container is OOMKilled" \
        TEST_ARGS='--kubelet-flags="--fail-swap-on=false"'
      ```
      Acceptable outcomes:
      1. Test passes outright (bug not reproduced on this kubelet build).
      2. Test passes with the pod-cgroup verification gated behind `Skipf` (bug reproduced; skip kicked in).
      Failure outside of the gated assertion is a real bug in the new test — fix it.
- [ ] Write/update tests (this task is the test addition).

## Questions

1. Restart policy of the OOM-kill pod in Task 2 — the bug report says "Run this command when the pod is in status OOMKilled", which is unambiguous for `RestartPolicy: Never` (pod stops in `Failed`/OOMKilled state and waits). With `Always`/`OnFailure` the kubelet will restart the container before the patch can be sent, racing the test. Which do you prefer?
   - Option A: `RestartPolicy: Never` — pod stays OOMKilled; easiest to trigger the bug deterministically. **Suggested.**
   - Option B: `RestartPolicy: OnFailure` — closer to a real workload, but the test must race the kubelet to patch before the restart; flaky.

2. How to gate the pod-cgroup assertion if it fails (Task 2) — the task description says "add a comment for the assert and SKIP for now":
   - Option A: Run the assertion, catch the error, and `e2eskipper.Skipf(...)` when it fails so the suite stays green and we get a signal when the bug is fixed (test will start failing the skip). **Suggested.**
   - Option B: Comment-out the assertion entirely and add a `// TODO` — simpler but loses the "auto-detect when fixed" property.
   - Option C: Always `e2eskipper.Skip(...)` the whole test until the bug is fixed — loses coverage of the resize-acceptance path which is independently valuable.

3. cgroup version — `podresize.VerifyPodCgroupValues` checks `memory.max` (cgroup v2). Should the test skip on cgroup v1 hosts?
   - Option A: Skip on v1 using `cgroups.IsPodOnCgroupv2Node` — the bug snippet uses `memory.max` (v2). **Suggested.**
   - Option B: Implement both v1 (`memory.limit_in_bytes`) and v2 paths — more code, more surface area, the helper already does the right thing per-container.
