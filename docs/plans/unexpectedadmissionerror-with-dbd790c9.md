# UnexpectedAdmissionError with initContainers — e2e_node regression test

## Overview
Issue kubernetes#138446: when a pod has both an init container and a regular
container requesting the same device plugin resource, and the init container has
already completed, restarting the kubelet causes the pod to fail with an
`UnexpectedAdmissionError`. The device manager tries to re-allocate for the
already-completed init container and fails because no healthy devices are
available yet at admission time.

PR kubernetes#139959 ("Deferred Pod Admission for Unregistered Device Plugins")
fixes this by deferring admission rather than rejecting the pod when devices are
not yet available. That fix is **already present in this branch** as commit
`b65fe6517fa` (it touches `pkg/kubelet/...`, `pkg/kubelet/cm/devicemanager`,
`pkg/kubelet/lifecycle`, etc., and added admission-deferral e2e tests to
`test/e2e_node/device_plugin_failures_test.go`).

What is still missing is the specific regression test for the
init-container + regular-container + kubelet-restart scenario described in the
issue. This plan adds that e2e_node test. Because the fix is already in the
branch, the new test is expected to pass on this branch (and would have failed
prior to commit `b65fe6517fa`).

## Context
- Files involved:
  - Modify: `test/e2e_node/device_plugin_test.go` — add one `ginkgo.It(...)`
    regression test inside the `testDevicePlugin` function, alongside the
    existing kubelet-restart tests (e.g. "Keeps device plugin assignments across
    kubelet restarts (no pod restart, no device plugin restart)" at ~line 357,
    and "will not attempt to admit the succeeded pod after the kubelet restart
    and device plugin removed" at ~line 473).
- Related patterns (all already available in `device_plugin_test.go`):
  - Helpers: `parseLog`, `restartKubelet`, `getLocalTestNode`, `getPodByName`,
    `BeTheSamePodStillRunning`, `checkPodResourcesAssignment`,
    `getV1NodeDevices`, `makeBusyboxPod`.
  - Constants: `sleepIntervalToCompletion` ("5s"), `sleepIntervalForever`
    ("24h"), `e2enode.SampleDeviceResourceName`, `e2enode.SampleDevsAmount`,
    `busyboxImage`.
  - Shared variable: `v1PodResources` (declared near line 135).
  - Imports already present: `k8s.io/apimachinery/pkg/util/uuid`,
    `k8s.io/kubectl/pkg/util/podutils`, `k8s.io/apimachinery/pkg/api/resource`,
    `metav1`, `v1`, `e2epod`, `e2enode`, `framework`, `gomega`, `ginkgo`, `fmt`,
    `time`. No new imports are required.
- Dependencies: none new. Uses the existing sample device plugin harness.

## Development Approach
- **Testing approach**: Follow the repository's existing e2e_node device plugin
  testing practices — reuse the established helpers and the `ginkgo.It` block
  style used by neighboring tests in `testDevicePlugin`.
- The new test is a `Serial` device-plugin e2e_node test (the suite already runs
  serially because it restarts the kubelet). It cannot run in a plain unit-test
  CI environment; validation here is restricted to compilation, vet, and format
  checks. Full execution requires the e2e_node harness with the sample device
  plugin and a restartable kubelet.
- Complete the task fully before stopping.
- **CRITICAL: all validation (build/vet/gofmt) must pass before finishing.**

## Implementation Steps

### Task 2: Add the init-container regression e2e_node test

Add a regression test to `test/e2e_node/device_plugin_test.go` that reproduces
issue kubernetes#138446: a pod with an init container and a regular container
both requesting the same sample device resource. The init container runs to
completion (using `sleepIntervalToCompletion`), the regular container runs
"forever" (using `sleepIntervalForever`). After verifying both got a device and
the regular container is running, restart the kubelet and assert that the same
pod instance keeps running (no `UnexpectedAdmissionError`, no pod restart) and
that the device assignment for the regular container is preserved (verified via
the podresources API).

**Files:**
- Modify: `test/e2e_node/device_plugin_test.go`

- [x] Add a new `ginkgo.It("Keeps device plugin assignments across kubelet
  restarts for pods with completed init containers", ...)` block inside
  `testDevicePlugin`, placed next to the other kubelet-restart tests (after the
  "will not attempt to admit the succeeded pod after the kubelet restart and
  device plugin removed" test is a good location). Base it on the example test
  in the task description / issue.
- [x] Construct the pod with `RestartPolicy: v1.RestartPolicyAlways`, one
  `InitContainer` ("init-1") with command sleeping for
  `sleepIntervalToCompletion`, and one regular `Container` ("regular-1") with
  command sleeping for `sleepIntervalForever`. Both request a single
  `e2enode.SampleDeviceResourceName` via `Limits`/`Requests`. Use the
  `podRECMD = "devs=$(ls /tmp/ | egrep '^Dev-[0-9]+$') && echo stub devices:
  $devs && sleep %s"` pattern and `busyboxImage`.
- [x] Create the pod with `e2epod.NewPodClient(f).CreateSync`, then use
  `parseLog` with `deviceIDRE = "stub devices: (Dev-[0-9]+)"` to confirm the
  init container ("init-1") obtained a device and completed, and the regular
  container ("regular-1") obtained a device and is running. Assert both device
  IDs are non-empty.
- [x] Restart the kubelet (`restartKubelet(ctx, true)`), wait for the node to be
  schedulable (`e2enode.WaitForAllNodesSchedulable`), and wait for the sample
  device capacity/allocatable to return to `e2enode.SampleDevsAmount` via
  `getLocalTestNode` + `e2enode.CountSampleDeviceCapacity` /
  `CountSampleDeviceAllocatable`.
- [x] Assert the same pod instance is still running after the restart using
  `gomega.Eventually(ctx, getPodByName).WithArguments(f, pod1.Name)...
  Should(BeTheSamePodStillRunning(pod1), ...)`.
- [x] Verify the post-restart device assignment for the regular container using
  the podresources API: refresh `v1PodResources` via `getV1NodeDevices`, then
  `checkPodResourcesAssignment(v1PodResources, pod2.Namespace, pod2.Name,
  pod2.Spec.Containers[0].Name, e2enode.SampleDeviceResourceName,
  []string{regDevID})` and `framework.ExpectNoError`.
- [x] Validate: `gofmt -l test/e2e_node/device_plugin_test.go` reports no diff;
  `go vet ./test/e2e_node/` passes; the package compiles via
  `go build ./test/e2e_node/...` (or `go test -c -tags e2e_node
  ./test/e2e_node/` if a build tag is required). Fix any compile/vet/format
  issues. (Running the test itself requires the e2e_node harness and is not
  automatable here.)
- [x] Commit the change with message: `feat: add e2e_node regression test for
  UnexpectedAdmissionError with completed init containers`.
