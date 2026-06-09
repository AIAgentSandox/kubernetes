# Remove Disruptive Tests Tag from Kubernetes

## Overview
The `[Disruptive]` test tag (and its companion API `framework.WithDisruptive()` / `Framework.WithDisruptive()`) has lost practical meaning — it does not select a coherent class of tests and is no longer useful for filtering. See https://github.com/kubernetes/kubernetes/pull/135142#discussion_r2507175483 for context. This plan removes the tag end-to-end: the framework helpers, the `"Disruptive"` Ginkgo label entries in the wrapper's bookkeeping sets, every test that wires the tag onto a `Describe`/`Context`/`It`/`SIGDescribe`/`ConformanceIt`, related docs/comments, the conformance walker test fixtures, and finally regenerates `conformance.yaml` so the two existing Conformance test names lose the `[Disruptive]` token.

## Context

- Files involved (framework + plumbing):
  - `test/e2e/framework/ginkgowrapper.go` — defines `WithDisruptive()`, `(*Framework).WithDisruptive()`, `withDisruptive()`, and lists `"Disruptive"` in both `leafNodeLabels` and `deprecatedTags`.
  - `test/e2e/framework/internal/unittests/bugs/bugs.go` — fixture calls `framework.WithDisruptive()` and golden `ListTestsOutput` / `ListLabelsOutput` strings embed `[Disruptive]` / `Disruptive`.
  - `test/conformance/walk_test.go` — `TestValidateTestName` cases include `[Disruptive]` in fixture test names.
  - `test/conformance/testdata/conformance.yaml` — generated; two `codename` entries include `[Disruptive]` and need regeneration.
  - `test/conformance/walk.go` — `regexIneligibleTags` does not include `Disruptive`; no change required, only used to confirm.
  - `test/e2e/storage/framework/testpattern.go` and `test/e2e/storage/framework/testsuite.go` — doc comments reference `framework.WithDisruptive()` as the example tag.
  - `test/e2e/storage/external/README.md` — example `ginkgo` invocations skip on `\[Disruptive\]`.
  - `test/e2e/reporters/progress.go` — example log line in comment contains `[Disruptive]`.
  - `test/e2e/node/node_problem_detector.go` — code comment mentions "not marked as Disruptive or Serial".
  - `test/e2e/storage/testsuites/disruptive.go` — `InitCustomDisruptiveTestSuite` wires `framework.WithDisruptive()` into the suite's `TestTags`. The disruptive storage *test suite* (suite name `"disruptive"`) is a domain concept and is not being removed; only the tag wiring is.
  - `test/e2e_node/lock_contention_linux_test.go` — has a code comment "Disruptive because the kubelet is restarted in the test." (keep prose intent, update wording).

- Files involved (test call sites — remove `framework.WithDisruptive()` / `f.WithDisruptive()` argument from each containing call): the 53 test files listed below carry one or more occurrences. The grep `framework.WithDisruptive()|f.WithDisruptive()` over `test/` is authoritative; the same set should be visited.
  - `test/e2e/apimachinery/apiserver_identity.go`, `test/e2e/apimachinery/etcd_failure.go`
  - `test/e2e/apps/daemon_restart.go`, `test/e2e/apps/statefulset.go`
  - `test/e2e/cloud/gcp/ha_master.go`, `test/e2e/cloud/gcp/network/kube_proxy_migration.go`, `test/e2e/cloud/gcp/node_lease.go`, `test/e2e/cloud/gcp/reboot.go`, `test/e2e/cloud/gcp/resize_nodes.go`, `test/e2e/cloud/gcp/restart.go`, `test/e2e/cloud/nodes.go`
  - `test/e2e/common/node/downwardapi.go`, `test/e2e/common/storage/downwardapi.go`
  - `test/e2e/instrumentation/logging/generic_soak.go`
  - `test/e2e/lifecycle/bootstrap/bootstrap_signer.go`
  - `test/e2e/network/networking.go`, `test/e2e/network/service.go`
  - `test/e2e/node/taints.go`
  - `test/e2e/storage/csimock/csi_kubelet_restart.go`, `test/e2e/storage/generic_persistent_volume-disruptive.go`, `test/e2e/storage/nfs_persistent_volume-disruptive.go`, `test/e2e/storage/non_graceful_node_shutdown.go`, `test/e2e/storage/pvc_storageclass.go`, `test/e2e/storage/static_pods.go`, `test/e2e/storage/volume_provisioning.go`
  - `test/e2e/storage/testsuites/disruptive.go`, `test/e2e/storage/testsuites/subpath.go`
  - `test/e2e/windows/eviction.go`, `test/e2e/windows/node_shutdown.go`, `test/e2e/windows/reboot_node.go`
  - `test/e2e_node/container_log_rotation_test.go`, `test/e2e_node/critical_pod_test.go`, `test/e2e_node/device_manager_test.go`, `test/e2e_node/device_plugin_multiple_test.go`, `test/e2e_node/device_plugin_test.go`, `test/e2e_node/dra_test.go`, `test/e2e_node/eviction_test.go`, `test/e2e_node/kubelet_config_dir_test.go`, `test/e2e_node/lock_contention_linux_test.go`, `test/e2e_node/memory_manager_test.go`, `test/e2e_node/mirror_pod_grace_period_test.go`, `test/e2e_node/os_label_rename_test.go`, `test/e2e_node/quota_lsci_test.go`, `test/e2e_node/restart_all_containers_test.go`, `test/e2e_node/restart_test.go`, `test/e2e_node/runtime_conformance_test.go`, `test/e2e_node/split_disk_test.go`, `test/e2e_node/system_node_critical_test.go`, `test/e2e_node/unknown_pods_test.go`

- Out of scope (do NOT modify):
  - `CHANGELOG/CHANGELOG-1.17.md` — historical release notes; leave untouched.

- Related patterns:
  - `framework.WithDisruptive()` is a variadic Ginkgo wrapper argument; removing it never changes the surrounding signature — just delete the argument and any now-trailing comma. Be careful to preserve other tags on the same line (`WithSerial`, `WithSlow`, `feature.*`, `WithFeatureGate`, etc.).
  - The framework wrapper already exposes `WithLabel`, `WithSerial`, `WithSlow`, `WithFeature`, `WithFeatureGate`, etc., and `tagRe` / `deprecatedTags` / `leafNodeLabels` are the canonical bookkeeping points — only `"Disruptive"` entries leave.
  - Conformance regeneration: `test/conformance/gen-conformance-yaml.sh` (invoked via the existing `update-conformance-yaml` make target) produces `test/conformance/testdata/conformance.yaml`; never hand-edit it.

- Dependencies: Standard Kubernetes build/test tooling (`make`, `go test`, `hack/verify-*` scripts, `test/conformance/gen-conformance-yaml.sh`). No new tooling.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices
  - Run `go build ./...` and `go vet ./test/...` after each significant batch of edits to catch leftover references quickly.
  - For framework/internal unit tests, run `go test ./test/e2e/framework/internal/unittests/...` and `go test ./test/conformance/...`.
  - Regenerate `test/conformance/testdata/conformance.yaml` via `test/conformance/gen-conformance-yaml.sh` (or the documented `make update-conformance-yaml`) — do not hand-edit the yaml.
  - Run `hack/verify-conformance-yaml.sh` (and any other verify scripts the repo runs in CI, e.g. `hack/verify-gofmt.sh`, `hack/verify-govet.sh`) before declaring a task done.
- Complete each task fully before moving to the next.
- **CRITICAL: all tests must pass before starting next task**

## Implementation Steps

### Task 1: Strip `framework.WithDisruptive()` / `f.WithDisruptive()` from all test call sites
Remove the tag from every test `Describe`/`Context`/`It`/`SIGDescribe`/`ConformanceIt`/`sigDescribe`/`utils.SIGDescribe` call across `test/e2e/...` and `test/e2e_node/...`. After this task, no production test code should compile with a reference to `WithDisruptive` other than the framework definitions themselves (which Task 4 deletes).

**Files:**
- Modify: All 53 call-site files listed in the Context section above (`test/e2e/...`, `test/e2e_node/...`). Treat the grep result for `\b(framework|f)\.WithDisruptive\(\)` as authoritative — every occurrence in non-comment code is to be removed.

- [x] For each call, delete the `framework.WithDisruptive()` (or `f.WithDisruptive()`) argument and tidy adjacent whitespace/commas; preserve all other tags, the literal text descriptor, the body function, and any `ginkgo.Ordered` / `feature.*` / `WithFeatureGate(...)` arguments.
- [x] In `test/e2e/storage/testsuites/disruptive.go`: drop only `framework.WithDisruptive()` from `TestSuiteInfo.TestTags`, keeping `framework.WithLabel("LinuxOnly")`. Do not rename the file, type, or `InitDisruptiveTestSuite` symbols — that is a domain-named storage suite, not the tag.
- [x] Where a callsite's only tags were `WithDisruptive()` (e.g. `framework.WithDisruptive()` solo on `SIGDescribe`/`utils.SIGDescribe`), check the resulting call still has either a string literal name first or another valid arg — Ginkgo requires the description; do not leave a bare `SIGDescribe(func() {...})`.
- [x] Update the prose comment at `test/e2e/storage/testsuites/subpath.go:963` ("Disruptive test run serially…") to a tag-neutral wording (e.g. "Run serially so we can…"); preserve the actual test logic.
- [x] Update the prose comment at `test/e2e_node/lock_contention_linux_test.go:37` (`// Disruptive because the kubelet is restarted in the test.`) to a wording that no longer implies a tag (e.g. "Restarts the kubelet during the test.").
- [x] Update the prose comment at `test/e2e/node/node_problem_detector.go:101` to drop the `Disruptive` reference (e.g. "This test is not marked as Serial…") while keeping the kept guidance about not restarting the kubelet.
- [x] Run `go build ./test/...` and `go vet ./test/...` to confirm the tree compiles with the call-site changes alone (the framework helper still exists and remains exported until Task 4).
- [x] Spot-check at least one test per directory family (e.g. one in `test/e2e/storage`, `test/e2e/network`, `test/e2e/cloud/gcp`, `test/e2e_node`) with `gofmt -d` to confirm no stray commas/whitespace.

### Task 2: Update the framework bugs unit-test fixture and golden output
The `bugs` unit test exercises the wrapper's bookkeeping (label sets, output rendering). Remove `framework.WithDisruptive()` from the fixture call and drop `[Disruptive]` / `Disruptive` from the embedded golden strings so the test still passes.

**Files:**
- Modify: `test/e2e/framework/internal/unittests/bugs/bugs.go`

- [x] Remove the `framework.WithDisruptive(),` line from the `framework.SIGDescribe("testing")("abc", …)` argument list inside `Describe()`.
- [x] In the `ListTestsOutput` constant, delete the ` [Disruptive]` token from both of the long expected lines (preserve all other tokens and their order — the ordering is alphabetical/trailing-leafNodeLabels and must match the wrapper's actual emission after Task 4).
- [x] In the `ListLabelsOutput` constant, delete the `    Disruptive\n` line.
- [x] Do not change the surrounding bug-output strings (`bugOutput`, `numBugs`, `bugsOutputCompare` etc.) unless the line numbers in `ERROR: bugs.go:NN` shift because of the deleted source line — if they do shift, update each affected line number to the new value precisely (this is the only `LineNumber` arithmetic in the test). [updated bugs.go:107→106, ListTestsOutput 101→100 and 96→95]
- [x] Run `go test ./test/e2e/framework/internal/unittests/bugs/...` and confirm it passes.

### Task 3: Update the conformance walk test fixture
`TestValidateTestName` has two fixture entries that use `[Disruptive]` as an example of a *valid* tag. Once the tag is gone these strings should also drop it so the fixture remains representative.

**Files:**
- Modify: `test/conformance/walk_test.go`

- [x] Edit the test case at `walk_test.go:183` so the input becomes `"a test case with valid tags [LinuxOnly] [NodeConformance] [Serial]"` (still no expected error).
- [x] Edit the test case at `walk_test.go:203`: drop ` [Disruptive]` from the input string; leave the `tagString` expectation `"[Alpha],[Flaky]"` unchanged (the case is verifying ineligible-tag detection, not Disruptive).
- [x] Run `go test ./test/conformance/...` (excluding the conformance.yaml diff check, which is handled in Task 5).

### Task 4: Delete `WithDisruptive` from the framework wrapper
With every caller removed, delete the public API and the wrapper's bookkeeping entries for `"Disruptive"`.

**Files:**
- Modify: `test/e2e/framework/ginkgowrapper.go`

- [x] Remove the line `"Disruptive",` from the `leafNodeLabels` set initializer (around line 278).
- [x] Remove `"Disruptive"` from the `deprecatedTags` set initializer (around line 430). Keep the other entries (`Conformance`, `Flaky`, `NodeConformance`, `Serial`, `Slow`) intact.
- [x] Delete the `WithDisruptive()` package function, the `(*Framework).WithDisruptive()` method, and the private `withDisruptive()` helper (lines 675–690) along with their preceding doc comment.
- [x] Update the doc comments in `test/e2e/storage/framework/testpattern.go:86` and `test/e2e/storage/framework/testsuite.go:83` so the example reads `framework.WithSlow()` (or another still-extant helper) instead of `framework.WithDisruptive()`.
- [x] Run `go build ./...` to confirm no stragglers. Any compile error here points to a missed call site in Task 1.
- [x] Run `go test ./test/e2e/framework/...` and `go test ./test/e2e/framework/internal/unittests/...`.

### Task 5: Regenerate the conformance YAML and clean docs/comments
The two Conformance tests in `test/e2e/node/taints.go` previously emitted `[Disruptive]` into their codenames; after Task 1 they no longer do, so the generated `conformance.yaml` will diverge. Regenerate it and clean up the remaining doc/comment references.

**Files:**
- Modify: `test/conformance/testdata/conformance.yaml` (via regeneration script, do **not** hand-edit)
- Modify: `test/e2e/storage/external/README.md`
- Modify: `test/e2e/reporters/progress.go`

- [x] Run `test/conformance/gen-conformance-yaml.sh` (or the documented make target — confirm by reading `test/conformance/README.md` if needed) and let it rewrite `test/conformance/testdata/conformance.yaml`. Confirm via `grep Disruptive test/conformance/testdata/conformance.yaml` that no entries remain. [ran hack/update-conformance-yaml.sh; only the two taints.go codenames changed]
- [x] In `test/e2e/storage/external/README.md`, update the two `ginkgo` example invocations (lines 40 and 50) so the `-skip` regex no longer references `\[Disruptive\]` (drop the `Disruptive` alternative; keep `\[Feature:...\]` where it appears).
- [x] In `test/e2e/reporters/progress.go`, update the comment example log line (around line 47) to a representative test name that does not embed `[Disruptive]` (e.g. swap to one of the post-cleanup codenames or simply drop the ` [Disruptive]` token from the example).
- [x] Run `hack/verify-conformance-yaml.sh` (and `hack/verify-gofmt.sh`, `hack/verify-govet.sh` if part of the standard local verify set) to confirm parity with CI. [verify-conformance-yaml PASS, verify-gofmt PASS, go vet ./test/e2e/reporters/... clean]
- [x] Final sanity sweep: `Grep` the repo for `Disruptive` and confirm the only remaining matches are in `CHANGELOG/CHANGELOG-1.17.md` (intentionally untouched). [plus the intentionally-kept domain-named storage suite `InitDisruptiveTestSuite` in disruptive.go/base.go per Question 1 Option A]

## Questions

1. Should `InitDisruptiveTestSuite` / `disruptiveTestSuite` in `test/e2e/storage/testsuites/disruptive.go` (and the imports of that suite in `test/e2e/storage/testsuites/base.go`) be renamed at the same time, since the suite name `"disruptive"` echoes the dropped tag?
   - Option A: Keep the suite name/types as-is — they describe the *behaviour* (kubelet restarts, etc.) and dropping the Ginkgo tag does not require renaming the storage suite.
   - Option B: Rename suite + file + types to something like `KubeletRestart` to mirror what the suite actually exercises.
   - Suggested: Option A — the task description names the *tag* (and its framework plumbing) as the thing to remove, and the storage suite name is a separate concern that would balloon the diff and review surface.

2. Should the existing `f.WithDisruptive()` annotations on the two `framework.ConformanceIt(...)` calls in `test/e2e/node/taints.go` be replaced with another tag (e.g. `f.WithSerial()` is already present, but should they get `f.WithSlow()` too)?
   - Option A: Just drop `f.WithDisruptive()` and keep the existing `Serial` from the surrounding `Context`/`Describe`; do not introduce a new tag.
   - Option B: Replace with `f.WithSlow()` to preserve some scheduling hint.
   - Suggested: Option A — the linked PR's discussion frames the tag as not carrying meaningful information, so adding a different tag opportunistically would conflict with the spirit of the cleanup.
