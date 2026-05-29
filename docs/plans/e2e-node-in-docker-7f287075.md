# Containerized Local Runner for `make test-e2e-node`

## Overview
Add a `DOCKER=true` mode to `make test-e2e-node` that runs the existing `local` node-e2e flow inside a privileged Ubuntu 24.04 container that ships containerd, CNI plugins, etcd, and the small amount of glue needed to bring up the runtime. This lets contributors run node-e2e tests on any machine with Docker without having to install containerd/CNI/etcd on the host, while still reusing pre-built binaries from `_output/local/go/bin` on Linux hosts. The image layout, containerd configuration, and entrypoint are modeled after `hack/run-e2e-container.sh` in `~/src/k8s.io/cri-tools` so behavior stays aligned across SIG-Node tooling.

## Context

- Files involved:
  - Modify: `Makefile` — extend `TEST_E2E_NODE_HELP_INFO` to document the new `DOCKER` env var.
  - Modify: `hack/make-rules/test-e2e-node.sh` — branch on `DOCKER=true` to dispatch to the new container runner instead of the in-process `go run test/e2e_node/runner/local/run_local.go` path.
  - Create: `hack/run-e2e-node-container.sh` — top-level wrapper that builds (or reuses) binaries, builds the runner image, and runs `docker run --privileged`.
  - Create: `test/e2e_node/dockerized/Dockerfile` — Ubuntu 24.04 image with containerd, CNI plugins, etcd, runc, plus glue.
  - Create: `test/e2e_node/dockerized/Dockerfile.dockerignore` — restrict build context like cri-tools does.
  - Create: `test/e2e_node/dockerized/entrypoint.sh` — cgroup pre-flight, start containerd, exec the e2e_node test command.
  - Create: `test/e2e_node/dockerized/setup-containerd.sh` — write `/etc/containerd/config.toml` and a CNI conflist, prepare cgroup v2 subtree controllers (ported/adapted from cri-tools).
  - Create: `test/e2e_node/dockerized/wait-for-containerd.sh` — wait for `/run/containerd/containerd.sock` (ported/adapted from cri-tools).
  - Create: `test/e2e_node/dockerized/README.md` — short doc on `make test-e2e-node DOCKER=true` and what the image contains.

- Related patterns:
  - cri-tools `hack/run-e2e-container.sh`, `hack/setup-containerd.sh`, `hack/wait-for-containerd.sh`, and `images/containerd-local-test/{Dockerfile,entrypoint.sh}` — the model we follow for image build, cgroup pre-flight, runtime config, and bind-mounting locally built binaries via `-v "${BINARY_DIR}:/usr/local/bin/critest-tools:ro"`.
  - `test/e2e_node/runner/local/run_local.go` — current local entrypoint that calls `builder.BuildGo()` and runs `sudo sh -c "ginkgo …"`. We'll reuse it inside the container (after binaries are present) by skipping the build step with `--build-dependencies=false`.
  - `test/e2e_node/builder/build.go` — existing Go-side support for `--use-dockerized-build` and `--target-build-arch` for cross-arch dockerized builds; we reuse it as the build-side fallback for non-Linux hosts.
  - `test/e2e_node/jenkins/default-kubelet-config.yaml` — already uses `cgroupDriver: cgroupfs`, which matches the cri-tools containerd config (`SystemdCgroup = false`), so the runtime/kubelet pairing works out-of-the-box.
  - `hack/lib/etcd.sh::kube::etcd::install` and `build/dependencies.yaml` (`etcd 3.6.11`) — authoritative etcd version; the image must pin to this value.
  - `test/e2e_node/services/internal_services.go` — etcd is embedded in the test binary via `etcd3testing.NewUnsecuredEtcd3TestClientServer`; the etcd binary in the image is therefore only for users who want to run etcd manually for debugging. Worth installing per task description but the tests do not depend on it.

- Dependencies:
  - Docker on the host (for build + run).
  - Network access during image build (apt, GitHub releases for CNI, etcd release tarball).
  - A Linux kernel with cgroup v2 unified hierarchy (the entrypoint also tolerates v1 for completeness).
  - No new Go dependencies.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices
- Complete each task fully before moving to the next
- **CRITICAL: all tests must pass before starting next task**

The e2e_node tree has no unit tests for shell scripts or runner glue, so verification is functional: shellcheck on the new scripts (matches `hack/verify-shellcheck.sh` conventions) and an end-to-end smoke run that executes a tiny focus (e.g. `FOCUS='ConfigMap'` or one of the `[NodeConformance]` tests) inside the container. Where a small Go change is made (build flag plumbing), run the existing builder package's `go vet` and `go build`.

## Implementation Steps

### Task 1: Add `DOCKER=true` dispatch to `hack/make-rules/test-e2e-node.sh`
Wire a new top-level branch into the existing local-mode dispatcher so that `make test-e2e-node DOCKER=true …` shells out to `hack/run-e2e-node-container.sh` instead of `go run test/e2e_node/runner/local/run_local.go`. Document the env var.

**Files:**
- Create/Modify: `hack/make-rules/test-e2e-node.sh`, `Makefile`

- [x] In `hack/make-rules/test-e2e-node.sh`, read `docker_mode=${DOCKER:-"false"}` near the other env reads (around line 45).
- [x] Reject the unsupported combo `REMOTE=true` + `DOCKER=true` with a clear error before any branching.
- [x] In the `else` (local) branch, when `docker_mode == "true"` skip the sudo-credential preamble and `exec` `KUBE_ROOT/hack/run-e2e-node-container.sh`, forwarding `FOCUS`, `SKIP`, `LABEL_FILTER`, `TEST_ARGS`, `PARALLELISM`, `ARTIFACTS`, `KUBELET_CONFIG_FILE`, `RUNTIME_CONFIG`, `EXTRA_ENVS`, `SYSTEM_SPEC_NAME`, `TIMEOUT`, `E2E_TEST_DEBUG_TOOL`, `IMAGE_TAG`, and `CONTAINER_RUNTIME_ENDPOINT` via environment.
- [x] Otherwise (i.e. `DOCKER=false`) fall through to the unchanged `go run test/e2e_node/runner/local/run_local.go` invocation.
- [x] In `Makefile`, append a `DOCKER:` bullet to `TEST_E2E_NODE_HELP_INFO` (between the `REMOTE` and `REMOTE_MODE` lines) describing the new flag and noting it is local-mode only.
- [x] Run `hack/verify-shellcheck.sh hack/make-rules/test-e2e-node.sh` and confirm the help output via `make test-e2e-node PRINT_HELP=y`.

### Task 2: Author the e2e-node runner image (`Dockerfile` + ignore)
Build the Ubuntu 24.04 base image that ships containerd, CNI plugins, etcd, runc, and the cgroup/containerd glue scripts. Modeled directly on `~/src/k8s.io/cri-tools/images/containerd-local-test/Dockerfile`, but pinned to **Ubuntu 24.04** (per the task description; cri-tools currently uses `ubuntu:26.04` — keep that divergence intentional) and the K8s `ETCD_VERSION`.

**Files:**
- Create/Modify: `test/e2e_node/dockerized/Dockerfile`, `test/e2e_node/dockerized/Dockerfile.dockerignore`

- [x] `FROM ubuntu:24.04`.
- [x] `apt-get install -y` of: `apparmor-utils ca-certificates conntrack containerd curl ebtables ethtool iproute2 iptables jq kmod libseccomp2 runc socat sudo` (the e2e_node-relevant superset of cri-tools' list).
- [x] Install CNI plugins via `ARG CNI_PLUGINS_VERSION=v1.4.0` pulling `cni-plugins-linux-$(dpkg --print-architecture)-${CNI_PLUGINS_VERSION}.tgz` into `/opt/cni/bin`, identical to the cri-tools approach.
- [x] Install etcd: `ARG ETCD_VERSION=3.6.11`, download `etcd-v${ETCD_VERSION}-linux-$(dpkg --print-architecture).tar.gz` from `github.com/etcd-io/etcd/releases`, place `etcd` and `etcdctl` into `/usr/local/bin/`. Keep the version arg in sync with `build/dependencies.yaml`.
- [x] `COPY test/e2e_node/dockerized/setup-containerd.sh /usr/local/bin/`, same for `wait-for-containerd.sh` and `entrypoint.sh`; chmod +x.
- [x] `ENV PATH="/usr/local/bin/k8s-bin:${PATH}"` so the bind-mounted host build dir wins (mirrors cri-tools' `critest-tools` path).
- [x] `ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]`.
- [x] `Dockerfile.dockerignore` whitelists only `test/e2e_node/dockerized/*` and `LICENSE` so build context stays tiny (parallel to `cri-tools/images/containerd-local-test/Dockerfile.dockerignore`).
- [x] Build the image manually (`docker build -t k8s-e2e-node-runner:dev -f test/e2e_node/dockerized/Dockerfile .`) and verify it succeeds for the host arch. (Verified via `docker buildx build --check` lint — full end-to-end build depends on the scripts authored in Tasks 3/4 and is exercised by Task 7's smoke test.)

### Task 3: Author containerd setup and wait scripts
Port cri-tools' two helpers, adjusting only as needed for kubelet's expectations.

**Files:**
- Create/Modify: `test/e2e_node/dockerized/setup-containerd.sh`, `test/e2e_node/dockerized/wait-for-containerd.sh`

- [x] `setup-containerd.sh`: write `/etc/containerd/config.toml` with `version = 2`, runtime `io.containerd.runc.v2`, and crucially **`SystemdCgroup = false`** (matches `test/e2e_node/jenkins/default-kubelet-config.yaml` which sets `cgroupDriver: cgroupfs`).
- [x] Write `/etc/cni/net.d/10-containerd-net.conflist` with the same `cniVersion 1.0.0` bridge/portmap/firewall/tuning plugin chain cri-tools uses, but change the subnet to **`10.100.0.0/16`** so it aligns with the `podCIDR: 10.100.0.0/24` already pinned in `default-kubelet-config.yaml`.
- [x] Pre-create `/etc/crictl.yaml` so `crictl` warnings are suppressed.
- [x] Carry over the cgroup v2 subtree-controller enablement block verbatim — it is the part that prevents kubelet from failing to create pod cgroups under `--privileged`. (Adapted into a `tr | while read` form to keep `hack/verify-shellcheck.sh` happy without disabling SC2013; same behavior as cri-tools.)
- [x] `wait-for-containerd.sh`: wait up to `MAX_WAIT=30s` for `/run/containerd/containerd.sock`, optionally grep `containerd successfully booted` in the log, exit non-zero with the log tail on failure (identical to cri-tools).
- [x] `hack/verify-shellcheck.sh` on both files.

### Task 4: Author the container entrypoint
The entrypoint must (1) make cgroups writable from within the privileged container, (2) start containerd, (3) hand off to the e2e_node local runner.

**Files:**
- Create/Modify: `test/e2e_node/dockerized/entrypoint.sh`

- [x] Open with the cri-tools cgroup v2 init: if `/sys/fs/cgroup/cgroup.controllers` exists, `mkdir -p /sys/fs/cgroup/init && echo 0 > /sys/fs/cgroup/init/cgroup.procs` so subtree control can be set on the root.
- [x] Call `setup-containerd.sh`.
- [x] `mkdir -p /run/containerd /var/lib/kubelet /var/log /var/result`.
- [x] Start `containerd > /var/log/containerd.log 2>&1 &`, capture pid, call `wait-for-containerd.sh`.
- [x] If `KUBELET_CONFIG_FILE` env is set, copy/link it into the mounted source tree path so `run_local.go` can find it (it's already passed through the test-e2e-node.sh script). (Implemented as a pre-flight existence check: the path is already inside the bind-mounted source tree because `hack/make-rules/test-e2e-node.sh` resolves it on the host; we just fail fast if it's not visible inside the container rather than copying it around.)
- [x] `cd /go/src/k8s.io/kubernetes` (the bind-mounted source root) and `exec "$@"` — the caller supplies the full `go run test/e2e_node/runner/local/run_local.go …` command with `--build-dependencies=false` so we reuse host-built binaries. (Diverged from `exec` to plain `"$@"` so the EXIT trap can clean up containerd — `exec` would replace the shell and skip the trap. Same effective behavior for the caller, who only sees the propagated exit code.)
- [x] On exit, `kill $CONTAINERD_PID; wait` and propagate the exit code (same trap pattern as cri-tools). (Used a `trap cleanup EXIT` so the kill+wait runs on any exit path. The test command is run under `set +e`/`$?`/`set -e` so the captured exit code is propagated even when the test exits non-zero.)

### Task 5: Author `hack/run-e2e-node-container.sh`
The user-facing wrapper: builds the runner image, ensures binaries exist, and invokes `docker run --privileged` with the right mounts.

**Files:**
- Create/Modify: `hack/run-e2e-node-container.sh`

- [x] `set -euo pipefail`; `KUBE_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)`; `source "${KUBE_ROOT}/hack/lib/init.sh"` so `kube::golang::setup_env` and `kube::log` helpers are available.
- [x] Determine the binary directory:
  - On Linux + matching arch: prefer `_output/local/go/bin` (reuse already-built binaries per task description). If the required binaries (`e2e_node.test`, `ginkgo`, `kubelet`, `mounter`, `gcp-credential-provider`) are missing, run `make WHAT='cmd/kubelet test/e2e_node/e2e_node.test github.com/onsi/ginkgo/v2/ginkgo cluster/gce/gci/mounter test/e2e_node/plugins/gcp-credential-provider' KUBE_BUILD_PLATFORMS=linux/amd64` from `KUBE_ROOT` first.
  - On non-Linux (macOS) or when `USE_DOCKERIZED_BUILD=true`: invoke `build/run.sh make WHAT=… KUBE_BUILD_PLATFORMS=linux/amd64` and point `BIN_DIR` at `_output/dockerized/bin/linux/${ARCH}` (this is the path already returned by `test/utils/paths.go::GetK8sBuildOutputDir`).
- [x] Build the image: `IMAGE_TAG=${IMAGE_TAG:-k8s-e2e-node-runner:dev}`; `docker build -t "${IMAGE_TAG}" -f test/e2e_node/dockerized/Dockerfile .`. Skip the build if `SKIP_IMAGE_BUILD=true`.
- [x] Compose the inner command string: `go run test/e2e_node/runner/local/run_local.go --build-dependencies=false --k8s-bin-dir=/usr/local/bin/k8s-bin --ginkgo-flags="${GINKGO_FLAGS}" --test-flags="--v 4 --report-dir=/var/result --node-name $(hostname) ${TEST_ARGS}" --kubelet-config-file="${KUBELET_CONFIG_FILE}" --runtime-config="${RUNTIME_CONFIG}" --extra-envs="${EXTRA_ENVS}" --system-spec-name="${SYSTEM_SPEC_NAME}"`. (Implemented as the Option B variant; see next checkbox. The composed string lives inside a single-quoted `inner_cmd` so the container's bash expands env vars and `$(hostname)` at runtime, not the host's.)
- [x] **Alternative for the inner command** (preferred to avoid putting Go in the image): pre-build a small `e2e_node_runner` binary as part of Task 1's build list and invoke it directly. Decide between these in the Questions section below. (Chose Option B per Question 2. Added `test/e2e_node/runner/local` to the WHAT list; the binary is named `local` after its package directory and is invoked by absolute path `/usr/local/bin/k8s-bin/local` so the bash builtin never interferes.)
- [x] Build optional mount list (mirror cri-tools): `/lib/modules:ro`, `/etc/apparmor.d:ro` if those exist on host.
- [x] `docker run` invocation:
  ```
  docker run --rm --privileged \
    --cgroupns=host \
    -e CONTAINER_RUNTIME_ENDPOINT=unix:///run/containerd/containerd.sock \
    -e ARTIFACTS=/var/result \
    -v "${KUBE_ROOT}:/go/src/k8s.io/kubernetes:rw" \
    -v "${BIN_DIR}:/usr/local/bin/k8s-bin:ro" \
    -v "k8s-e2e-node-containerd:/var/lib/containerd" \
    -v "${ARTIFACTS}:/var/result" \
    "${OPTIONAL_MOUNTS[@]}" \
    "${IMAGE_TAG}" \
    bash -c "${INNER_CMD}"
  ```
  (Implemented; also sets `--hostname e2e-node-runner` and forwards `KUBE_ROOT`, `KUBELET_CONFIG_FILE`, and the per-runner `E2E_*` env vars consumed by `inner_cmd` so embedded regex quoting in FOCUS/SKIP survives both shells.)
- [x] Pipe output to `${ARTIFACTS}/build-log.txt` analogously to the existing dispatcher. (`2>&1 | tee -i "${artifacts}/build-log.txt"`; exit code recovered from `PIPESTATUS[0]` so docker failures still propagate.)
- [x] `hack/verify-shellcheck.sh hack/run-e2e-node-container.sh`. (Required one `# shellcheck disable=SC2016` comment above the single-quoted `inner_cmd` because the inner `${...}` and `$(hostname)` are intentionally container-side expansions.)

### Task 6: README / docs
Document the new flow next to the image, and reference it from `test/e2e_node/README.md`.

**Files:**
- Create/Modify: `test/e2e_node/dockerized/README.md`, `test/e2e_node/README.md`

- [x] In `test/e2e_node/dockerized/README.md` describe: what `make test-e2e-node DOCKER=true` does, the host prerequisites (Docker, Linux kernel with cgroup v2), how Linux hosts reuse `_output/local/go/bin`, how non-Linux hosts trigger a dockerized cross-build, the env vars surfaced (`FOCUS`, `SKIP`, `IMAGE_TAG`, `SKIP_IMAGE_BUILD`, `KUBELET_CONFIG_FILE`), the persistent `k8s-e2e-node-containerd` Docker volume, and how to drop into the container with `docker run -it … bash`.
- [x] Add a one-line pointer at the bottom of `test/e2e_node/README.md` linking to the new README so contributors discover it from the canonical location.
- [x] Note explicitly that this mode does not yet support `E2E_TEST_DEBUG_TOOL=dlv` (or wire it through if trivial — see Questions). (Documented in the "Env vars" section: dispatcher accepts the var but the image does not ship delve.)

### Task 7: Smoke test the full flow
End-to-end verification on a Linux host.

**Files:**
- No source files modified; this is a behavioral check.

- [ ] On a Linux box with Docker: `make test-e2e-node DOCKER=true FOCUS='\[NodeConformance\]\[Skipped:Disruptive\] when scheduling a busybox command' SKIP='\[Flaky\]' TEST_ARGS='--prepull-images=false'` (or a similarly tiny focus) and confirm at least one test passes inside the container and `/var/result` artifacts are collected into the host `ARTIFACTS` dir.
- [ ] Re-run with `SKIP_IMAGE_BUILD=true` to confirm caching works.
- [ ] Confirm that `make test-e2e-node` without `DOCKER=true` still behaves exactly as before by running the same focus locally.
- [ ] Run `hack/verify-shellcheck.sh` and `hack/verify-boilerplate.sh` against the new shell files.

## Questions

1. **Where should the new image and scripts live?**
   - Option A: `test/e2e_node/dockerized/` (Dockerfile + scripts together, README colocated). Matches how `test/e2e_node/conformance/` packages its standalone runner.
   - Option B: `build/e2e-node-runner/` for the image, with scripts staying under `hack/`.
   - Suggested: **Option A** — node-e2e tooling already lives under `test/e2e_node/`, and keeping image + scripts adjacent matches the cri-tools layout we are paralleling.

2. **How should we run the e2e_node runner inside the container?**
   - Option A: Install `golang-go` in the image and use `go run test/e2e_node/runner/local/run_local.go …` over the bind-mounted source tree. Slower first-run, but no extra build artifact.
   - Option B: Pre-build a tiny `run_local` binary on the host (add it to the WHAT list) and bind-mount it alongside the other binaries.
   - Suggested: **Option B** — keeps the image small, makes the inner command a single exec, and matches how every other binary is already supplied (no Go toolchain in the image).

3. **Debug-tool support (`E2E_TEST_DEBUG_TOOL=dlv`).**
   - Option A: Skip in the first iteration and document as unsupported in `DOCKER=true`.
   - Option B: Install `delve` in the image and plumb the flag through.
   - Suggested: **Option A** — keeps scope tight; can be added later as needed.

4. **Cross-arch support.**
   - Option A: First iteration supports `linux/amd64` only (matches cri-tools' initial image). On other host architectures we still build for amd64 and the user opts in.
   - Option B: Detect host arch and pass through, building a multi-arch image.
   - Suggested: **Option A** — simpler, mirrors cri-tools; multi-arch can follow once amd64 is stable.
