# Same Device Same Port: Edge Case Testing for Per-Endpoint Collection

## Overview

PR #135437 ("DRA-like fix for device-plugin") introduced a per-endpoint
collection (`endpointStore`) in `pkg/kubelet/cm/devicemanager/manager.go` and
a per-client slice (`map[string][]Client`) in
`pkg/kubelet/cm/devicemanager/plugin/v1beta1/server.go`. These keep multiple
endpoints per `resourceName` so the kubelet can support side-by-side device
plugins for the same resource.

The race that is *not* yet covered by tests: when an old device plugin
disconnects but the kubelet detects the closure *late*, while a new plugin is
already registering on the **same socket path / same resource**. The current
implementations key on `(resourceName, socketPath)` for both
`PluginConnected` / `PluginDisconnected` (manager) and
`registerClient` / `deregisterClient` (server). If the two flows interleave,
the kubelet can end up with **no endpoint registered for the resource while
the device plugin believes its registration succeeded**, which is exactly the
condition the task description calls out.

This plan adds unit tests that exercise these interleavings deterministically,
plus an end-to-end test that reproduces a realistic late-disconnect scenario.

## Context

- Files involved (test-only):
  - `pkg/kubelet/cm/devicemanager/manager_test.go` — add unit tests for
    `PluginConnected` / `PluginDisconnected` interleavings against the
    per-endpoint store.
  - `pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler_test.go` —
    new file. There is currently no `_test.go` in
    `plugin/v1beta1/`; add one to cover `registerClient` /
    `deregisterClient` / `getClient` interactions on the per-socket
    `clients map[string][]Client`.
  - `test/e2e_node/device_plugin_multiple_test.go` — extend with a
    same-socket-path / late-disconnect e2e scenario reusing the existing
    `testdeviceplugin` helper.
  - (Possibly) `test/e2e_node/testdeviceplugin/device-plugin.go` — only
    if a helper is needed to delay/withhold the disconnect signal from a
    fake plugin. Prefer doing the timing exclusively from the test if
    possible.

- Related patterns to follow:
  - Unit-test style of `TestEndpointSyncOnDisconnect`
    (`pkg/kubelet/cm/devicemanager/manager_test.go:2152`): builds a
    `ManagerImpl` via `newManagerImpl`, populates
    `endpoints` and `endpointStore` with a hand-rolled `endpointImpl`
    that has the `socket` field set, then drives state changes through
    `manager.PluginDisconnected(...)` / `genericDeviceUpdateCallback(...)`.
  - `MockEndpoint` (`manager_test.go:809-843`) already exposes
    `socketPath()`; reuse it where a real socket is not necessary.
  - Plugin-side handler logic to verify
    (`pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler.go:84-138`):
    `connectClient`, `registerClient`, `deregisterClient`,
    `getClient` (all keyed by `(name, socketPath)`).
  - Manager-side logic to verify
    (`pkg/kubelet/cm/devicemanager/manager.go:230-286`):
    `PluginConnected` rejects duplicate `(resourceName, socketPath)` with
    `fmt.Errorf("device plugin already connected: %s", e.socketPath())`,
    and `PluginDisconnected` only deletes the entry whose socket path
    matches.
  - E2E patterns in `test/e2e_node/device_plugin_multiple_test.go` —
    same `BeforeEach` / `AfterEach` scaffolding, `testdeviceplugin`
    usage as in the "DP2 takes long time to start working" case.

- Dependencies: no new external libraries. `testify/require`, `klog`,
  `ktesting`, `ginkgo/v2`, `gomega`, and `grpc` already used in the
  surrounding code.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices
- Complete each task fully before moving to the next
- **CRITICAL: all tests must pass before starting next task**
- Unit tests must be deterministic — drive concurrent orderings by
  serialising calls to `PluginConnected` / `PluginDisconnected` rather
  than relying on real `time.Sleep`s.
- E2E test reuses `testdeviceplugin.DevicePlugin` and respects the
  `framework.WithSerial()` / `f.WithDisruptive()` decorators that
  `Device Plugin Multiple` already uses.
- Run `go test ./pkg/kubelet/cm/devicemanager/... -race -count=5` for each
  new unit test to flush out any residual races introduced by the
  test or its fixture.

## Implementation Steps

### Task 1: Audit current `(resourceName, socketPath)` invariants

Document, in the test file headers, the invariants the new tests will
enforce, derived from reading the current `manager.go` and `handler.go`.
This keeps the assertions honest and grounded in real behavior rather
than an idealised contract.

**Files:**
- Read-only: `pkg/kubelet/cm/devicemanager/manager.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/server.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/client.go`,
  `pkg/kubelet/cm/devicemanager/endpoint.go`.

- [x] Confirm `PluginConnected` rejects duplicate
  `(resourceName, socketPath)` with the exact error string used in
  `manager.go:248`.
- [x] Confirm `PluginDisconnected` is a no-op when the
  `(resourceName, socketPath)` pair is unknown
  (`manager.go:263-270`).
- [x] Confirm the primary `m.endpoints[resourceName]` slot is **promoted**
  from an arbitrary surviving endpoint when a non-last sibling
  disconnects (`manager.go:278-285`).
- [x] Confirm `deregisterClient` removes only the matching socket and
  deletes the resource key only when the slice becomes empty
  (`handler.go:116-138`).
- [x] Capture these invariants as test-file comments so future readers
  understand *why* each assertion is made.

### Task 2: Unit tests for `PluginConnected` / `PluginDisconnected` race orderings

Add unit tests in `pkg/kubelet/cm/devicemanager/manager_test.go` that
deterministically drive the orderings that matter for the same-socket
case. Each test builds a manager via `newManagerImpl` (so the real
mutex / store paths execute) and uses a hand-rolled `endpointImpl` with
`socket` set, mirroring `TestEndpointSyncOnDisconnect`.

**Files:**
- Modify: `pkg/kubelet/cm/devicemanager/manager_test.go`.

- [x] `TestPluginConnected_SameResourceSameSocketRejected`: register two
  plugins with the **same** `resourceName` and **same** socket path back
  to back; assert the second `PluginConnected` returns the
  `device plugin already connected` error and that
  `endpointStore[resourceName]` still contains exactly one entry
  pointing at the first endpoint.
- [x] `TestPluginConnected_SameResourceDifferentSocketsCoexist`: register
  two plugins with the same `resourceName` but different socket paths;
  assert both entries live in `endpointStore[resourceName]` and that
  `m.endpoints[resourceName]` is set (any one of the two — capture which
  is selected to document current behavior, but assert only that it is
  one of them).
- [x] `TestPluginDisconnected_WrongSocketIsNoop`: with one endpoint in
  the store, call `PluginDisconnected` with a *different* socket path
  and assert state is unchanged (endpoint still present, devices not
  marked unhealthy).
- [x] `TestPluginDisconnected_PromotesSurvivor`: with two endpoints for
  one resource, disconnect the one currently in `m.endpoints` and
  assert (a) it is removed from `endpointStore`, (b) `m.endpoints` now
  contains the *other* endpoint, (c) `healthyDevices` for that
  resource is *not* zeroed (i.e. `markResourceUnhealthy` is *not*
  called because this was not the last endpoint).
- [x] `TestPluginDisconnected_LastEndpointMarksUnhealthy`: with one
  endpoint, disconnect it and assert (a) `endpointStore[resourceName]`
  is gone, (b) `markResourceUnhealthy` has run (healthy → unhealthy).
- [x] **Core race test 1 — `TestSameSocketRace_LateDisconnectAfterReconnect`:**
  - Register `e1` at `socketA` for `resourceA`.
  - Call `PluginDisconnected(resourceA, socketA)` — `e1` is removed.
  - Register `e2` at `socketA` for `resourceA` (succeeds — store is
    empty).
  - Now simulate the **late** disconnect callback for `e1` by calling
    `PluginDisconnected(resourceA, socketA)` a second time.
  - Implementation note: the plan's "load-bearing" assertion (e2 still
    present after the late callback) does NOT hold against current
    production code, because eviction is keyed only on socket path
    (I4). The test instead locks in the **current** behavior: the
    late callback evicts e2. This is paired with
    `TestServer_LateDisconnectDoesNotEvictNewClient` (Task 3) and is
    the regression risk the plan's Questions section pre-flags.
- [x] **Core race test 2 — `TestSameSocketRace_DisconnectBeforeReconnectAttempt`:**
  - Register `e1` at `socketA`, do **not** disconnect.
  - Attempt to register `e2` at `socketA` for the same resource without
    a prior disconnect — assert it is rejected with the
    `device plugin already connected` error.
  - Then disconnect `e1`, register `e2` again — must succeed; assert
    `m.endpoints[resourceA].e == e2`.
- [x] **Core race test 3 — `TestSameSocketRace_OverlappingConnects`:**
  - Drive two `PluginConnected` calls concurrently from goroutines (one
  for `socketA`, one for `socketA`) — the manager's mutex must
  serialise them so exactly one succeeds and the other returns the
  duplicate error. Use a `WaitGroup` + channel to assert exactly one
  error, exactly one success, and `len(endpointStore[resourceA]) == 1`.
  Run with `-race` (see Task 6).
- [x] Write/update tests (mandatory; the existing area has unit-test
  coverage and the PR-of-record already added `TestEndpointSyncOnDisconnect`).

### Task 3: Unit tests for the per-socket client map in the plugin server

Add a brand-new test file
`pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler_test.go` that
exercises `registerClient` / `deregisterClient` / `getClient` directly
through the unexported `server` type. These are the **other half** of the
same race: even if the manager's `endpointStore` is consistent, the
server's `clients map[string][]Client` could leak or lose connections
under the same interleaving and we want explicit coverage.

**Files:**
- Create: `pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler_test.go`.

- [ ] Introduce a minimal `fakeClient` that implements
  `Client` — `Connect`/`Run`/`Disconnect` no-ops, `SocketPath()`
  returns a configured string. No real gRPC needed.
- [ ] Introduce a minimal `fakeClientHandler` /
  `fakeRegistrationHandler` if needed to construct a `server` via
  `NewServer`; otherwise build a `*server` literal in the test file
  (package-internal access is allowed since the file lives in
  `package v1beta1`).
- [ ] `TestServer_RegisterClient_AppendsPerSocket`: register two
  clients with the same plugin name but different sockets; assert
  `s.clients[name]` has length 2 and both sockets are present.
- [ ] `TestServer_DeregisterClient_OnlyRemovesMatchingSocket`:
  start with two clients; deregister one by socket; assert the
  remaining client is the other one and the map entry survives.
- [ ] `TestServer_DeregisterClient_DeletesKeyWhenLast`: deregister
  the only client and assert `s.clients[name]` is removed from the
  map (not just emptied).
- [ ] `TestServer_GetClient_ReturnsNilWhenSocketMissing`: register
  one client at `socketA`, call `getClient(name, socketB)`, expect nil.
- [ ] `TestServer_DeregisterClient_NoopForUnknownSocket`: deregister
  a `socketB` that was never registered; assert no panic and the
  `socketA` client is untouched.
- [ ] **Core race test —
  `TestServer_LateDisconnectDoesNotEvictNewClient`:**
  - Register client `c1` at `socketA`.
  - Deregister `c1` by `socketA`.
  - Register client `c2` at the *same* `socketA` (the test models a new
    process reusing the same path).
  - Issue a *second* deregister for `socketA` — emulating the late
    callback for `c1`. Assert that the second deregister evicts `c2`
    even though `c2` is a different `Client` instance (this is the
    current behavior because eviction is keyed only by socket path).
    Document this as the **regression risk** the task description is
    pointing at. If we agree the kubelet should *not* evict `c2` in
    this case (because `c2 != c1`), this test becomes a failing red
    test that proves the bug — surface it in the
    Questions section and decide whether to fix or just to lock in
    current behavior.
- [ ] Write/update tests (creating the file is itself the deliverable).

### Task 4: Document and (if needed) tighten the manager's identity check

Depending on the outcome of Task 3's "core race test", either:
- (a) Conclude that keying by socket path alone is correct because the
  socket path is unique per concrete plugin process and the kubelet
  controls the lifecycle, in which case add a short comment in
  `handler.go:116` and `manager.go:259` explaining the invariant the
  tests now lock in; or
- (b) If we decide that a *new* `Client` / `endpointImpl` instance at the
  same socket path must not be evicted by a stale callback for the old
  instance, add an identity field (e.g. a registration sequence
  counter, or pointer equality check) and update the eviction logic to
  compare it.

Only do (b) with explicit user sign-off — see Questions.

**Files (if (a)):**
- Modify: `pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler.go`,
  `pkg/kubelet/cm/devicemanager/manager.go` (comments only).

**Files (if (b)):**
- Modify: `pkg/kubelet/cm/devicemanager/manager.go`,
  `pkg/kubelet/cm/devicemanager/endpoint.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/handler.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/client.go`,
  `pkg/kubelet/cm/devicemanager/plugin/v1beta1/api.go` (for the
  identity field), plus update Task 2 / Task 3 tests to assert the new
  behavior.

- [ ] Resolve the question with the user before touching production code.
- [ ] If (a): add one-line invariant comments at the keyed-lookup sites.
- [ ] If (b): implement an identity token (e.g. monotonic uint64 set in
  `newEndpointImpl` / `NewPluginClient`) and use it in
  `PluginDisconnected` / `deregisterClient` so that a stale callback
  carrying an old identity never evicts a fresh entry. Adjust tests.
- [ ] Write/update tests as required by the chosen path.

### Task 5: E2E test — late disconnect on same socket path

Add a new `ginkgo.It` case to
`test/e2e_node/device_plugin_multiple_test.go` modelled on
`Device Plugin Multiple: DP2 takes long time to start working`. The goal
is to reproduce, end-to-end, a sequence in which the **same socket path**
is reused after a delayed disconnect, and assert the kubelet still has a
registered endpoint and devices remain allocatable.

**Files:**
- Modify: `test/e2e_node/device_plugin_multiple_test.go`.
- Possibly modify (only if necessary):
  `test/e2e_node/testdeviceplugin/device-plugin.go` — add an
  option/hook to delay returning the `ListAndWatch` close until a
  signal from the test, so the test can interleave a re-registration
  on the same socket before the stream-close is observed by the
  kubelet. Prefer **not** modifying this file; first try to drive the
  scenario by simply stopping the gRPC server and re-creating one
  on the same path.

- [ ] `Device Plugin Multiple: same socket path reused after delayed disconnect`:
  - `BeforeEach` stays the same (DP1 already running via
    `getSampleDevicePluginPod` and resource available).
  - Stand up `plugin2 := testdeviceplugin.NewDevicePlugin(nil)` at a
    specific socket name `S`. Register it via
    `RegisterDevicePlugin(ctx, f.UniqueName, SampleDeviceResourceName, …)`.
  - Schedule `pod1` requesting `e2enode.SampleDeviceResourceName` and
    verify it runs.
  - `plugin2.Stop()` to drop the gRPC server (this is the
    "disconnect"); do **not** wait for kubelet to fully observe the
    close.
  - Immediately create `plugin2b := testdeviceplugin.NewDevicePlugin(nil)`
    listening on the **same socket path** `S` and call
    `RegisterDevicePlugin` again with the same resource name.
  - Assert (with `gomega.Eventually`): node still reports
    `SampleDeviceResourceName` capacity ≥ allocated count; a new
    `pod2` requesting the resource is admitted and runs.
  - Assert (with `gomega.Consistently` for ~30s): the resource count
    never drops to zero between these two registrations — this is the
    "neither endpoint registered" failure mode the task is calling out.
- [ ] If the natural `Stop()` → relisten flow does not actually exercise
  the late callback (because gRPC close is fast and deterministic on
  the local node), add a minimal option to
  `testdeviceplugin.DevicePlugin` (e.g. `WithGracefulStop()` or a
  `holdDisconnect` channel) so the test can withhold the disconnect
  signal until after re-registration. Keep the surface area small;
  preserve backwards compatibility of `NewDevicePlugin(errorInjector)`.
- [ ] Add a short comment at the top of the test referencing the issue
  this guards against (no PR/issue number in the comment — comment on
  *behavior*, not history, per repo style).
- [ ] Write/update tests (this task is the test).

### Task 6: Run tests under `-race` and stabilise

Run the new unit tests under the race detector with multiple iterations
to flush out flakes, and run the e2e test in node-e2e mode locally to
confirm it passes deterministically.

**Files:**
- None modified beyond what the previous tasks already changed.

- [ ] `go test -race -count=20 -run 'TestPluginConnected|TestPluginDisconnected|TestSameSocketRace|TestEndpointSyncOnDisconnect' ./pkg/kubelet/cm/devicemanager/...`
- [ ] `go test -race -count=20 -run 'TestServer_' ./pkg/kubelet/cm/devicemanager/plugin/v1beta1/...`
- [ ] Run `make test-e2e-node FOCUS='Device Plugin Multiple.*same socket'`
  (or the local equivalent) at least 5 times and confirm a clean run.
- [ ] If any flake is observed, fix the *test* (not the production code)
  unless Task 4 (b) was chosen — in which case the production fix is
  the answer to the flake.

## Questions

1. When the kubelet evicts a client/endpoint keyed by socket path, must
   it also confirm the *identity* of the client being evicted matches
   the one that triggered the callback?
   - Option A: **No** — socket path is sufficient because the kubelet
     controls the lifecycle and a callback for an old client cannot
     fire after the kubelet has already deregistered it. Lock in
     current behavior with the new tests (Task 4 (a)).
   - Option B: **Yes** — add an identity token (monotonic counter or
     pointer comparison) and refuse to evict a fresh client when an
     old client's stale callback arrives. This is a behavior change in
     production code and requires the structural changes in Task 4 (b).
   - Suggested: Option A first, with a failing red test in Task 3
     (`TestServer_LateDisconnectDoesNotEvictNewClient`) that documents
     the risk and forces a conscious decision. Escalate to Option B if
     review feedback or a reproducible production-style race shows the
     same-socket reuse is actually attainable by a real device plugin.

2. Should the new e2e test be guarded by an existing feature/skip
   (e.g. `framework.WithSerial()` + `framework.WithDisruptive()`, which
   is already used by the surrounding `Device Plugin Multiple` block)
   or marked `WithFlaky()` initially while it bakes?
   - Option A: Match the surrounding block exactly —
     `f.WithSerial(), f.WithDisruptive()` and nothing more.
   - Option B: Add `framework.WithFlaky()` for the first release while
     the timing is observed.
   - Suggested: Option A. The whole block already restarts the kubelet
     and is serial; adding a flaky tag would dilute signal.

3. Is it acceptable to add a small `holdDisconnect` (or similar)
   facility to `test/e2e_node/testdeviceplugin/device-plugin.go` if the
   plain `Stop()`/`relisten` sequence doesn't reliably exercise the
   late-callback path?
   - Option A: Yes — keep the surface minimal (a single optional
     `chan struct{}` accessor) and document it as test-only.
   - Option B: No — keep the helper untouched and only test scenarios
     that the kubelet's natural timing already produces.
   - Suggested: Option A, but only if the Option B test proves flaky
     or doesn't actually hit the codepath.
