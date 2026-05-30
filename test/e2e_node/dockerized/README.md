# Containerized node-e2e runner (`make test-e2e-node DOCKER=true`)

This directory packages a privileged Docker image that runs the existing
node-e2e local flow (`go run test/e2e_node/runner/local/run_local.go`)
without requiring containerd, CNI plugins, runc, or etcd to be installed
on the host. It is the implementation behind:

```
make test-e2e-node DOCKER=true [FOCUS=… SKIP=… TEST_ARGS=… …]
```

The image and scripts are intentionally modeled on cri-tools'
`hack/run-e2e-container.sh` + `images/containerd-local-test/` so the two
SIG-Node tools stay aligned.

## Host prerequisites

- Docker (any recent version that supports `--privileged --cgroupns=host`).
- A Linux kernel with the cgroup v2 unified hierarchy. cgroup v1 is not
  supported — the entrypoint's cgroup pre-flight is gated on
  `/sys/fs/cgroup/cgroup.controllers` and containerd is configured for v2
  only.
- Sufficient privilege to run `docker run --privileged`.
- Outbound network access to `github.com` (CNI plugin + etcd release
  downloads) during the first `docker build`. Subsequent runs can use
  `SKIP_IMAGE_BUILD=true`.

No host install of containerd, CNI plugins, runc, or etcd is required —
those live inside the image.

> **Warning — host cgroup conflicts.** The runner uses
> `--cgroupns=host` and the bundled kubelet config sets `cgroupRoot: /`,
> so the in-container kubelet writes pod cgroups under the host's cgroup
> tree. On a workstation that is also running another kubelet, kind,
> minikube, k3s, or Docker Desktop, those processes will fight over
> `/sys/fs/cgroup/kubepods*` and `kubeReserved`/`systemReserved` may
> push host services around. Use `DOCKER=true` on dedicated/CI hosts
> or after stopping any other kubelet/runtime on the host.

## What happens when you run it

`hack/make-rules/test-e2e-node.sh` dispatches `DOCKER=true` to
`hack/run-e2e-node-container.sh`, which:

1. Ensures the node-e2e binaries (`kubelet`, `e2e_node.test`, `ginkgo`,
   `mounter`, `gcp-credential-provider`, and the small `local` runner
   binary from `test/e2e_node/runner/local`) exist on the host.
2. Builds (or reuses) the runner image
   `k8s-e2e-node-runner:dev` from `test/e2e_node/dockerized/Dockerfile`.
3. Runs the image with `--privileged --cgroupns=host`, bind-mounts the
   host source tree at `/go/src/k8s.io/kubernetes`, bind-mounts the
   binary directory at `/usr/local/bin/k8s-bin`, and exec's the runner
   binary with `--build-dependencies=false` so the host-built binaries
   are reused inside the container.

The entrypoint starts containerd, waits for `/run/containerd/containerd.sock`,
and then hands off to the runner.

## Where the binaries come from

- **Linux + matching host arch** (the common case): binaries are reused
  from `_output/local/go/bin`. If any required binary is missing,
  `run-e2e-node-container.sh` invokes `make WHAT=… KUBE_BUILD_PLATFORMS=linux/amd64`
  once to populate them. This matches the task description's goal of
  reusing pre-built binaries on Linux hosts.
- **Non-Linux hosts (macOS) or `USE_DOCKERIZED_BUILD=true`**: the script
  invokes `build/run.sh make WHAT=…` to cross-build `linux/amd64`
  binaries into `_output/dockerized/bin/linux/amd64`, then bind-mounts
  that directory instead.

Only `linux/amd64` is supported in the first iteration; pass
`TARGET_BUILD_ARCH=linux/arm64` to opt in to a different arch via the
dockerized build path (multi-arch images are not yet built).

## Env vars surfaced by `DOCKER=true`

In addition to the regular `make test-e2e-node` env vars (forwarded by
`hack/make-rules/test-e2e-node.sh`):

| Var | Default | Purpose |
| --- | --- | --- |
| `FOCUS` | `""` | Ginkgo focus regex. |
| `SKIP` | `\[Flaky\]\|\[Slow\]\|\[Serial\]` (when `LABEL_FILTER` is empty) | Ginkgo skip regex. |
| `LABEL_FILTER` | `""` | Ginkgo label query. |
| `PARALLELISM` | `8` | Forwarded as ginkgo `-nodes`. Set to `1` to disable parallel execution. |
| `RUN_UNTIL_FAILURE` | `false` | Adds ginkgo `--until-it-fails=true` when `true`. |
| `TIMEOUT` | `24h` | Replaces the ginkgo `-timeout` value. |
| `TEST_ARGS` | `""` | Extra flags forwarded to `e2e_node.test`. |
| `KUBELET_CONFIG_FILE` | `test/e2e_node/jenkins/default-kubelet-config.yaml` | Path (inside the bind-mounted source tree) to the kubelet config. Must resolve under `/go/src/k8s.io/kubernetes`. |
| `ARTIFACTS` | `/tmp/_artifacts/<timestamp>` | Host directory mounted at `/var/result` for junit / logs. |
| `IMAGE_TAG` | `k8s-e2e-node-runner:dev` | Tag used by `docker build` / `docker run`. |
| `SKIP_IMAGE_BUILD` | `false` | If `true`, skip `docker build` and reuse the existing tag. Useful for iterating on tests. |
| `USE_DOCKERIZED_BUILD` | auto | Force the dockerized cross-build even on a matching Linux host. |
| `TARGET_BUILD_ARCH` | `linux/amd64` | Target arch for the dockerized build path. |

`E2E_TEST_DEBUG_TOOL=dlv` / `gdb` is **not** supported in `DOCKER=true` —
the image does not ship `delve` or `gdb`, so the dispatcher rejects the
combination up-front with a clear error.

## Running on macOS (Apple Silicon)

Docker Desktop on macOS ARM64 works but requires extra flags:

### One-time setup

Install GNU coreutils and GNU tar (the build scripts require them):

```bash
brew install coreutils gnu-tar
```

Add their gnubin directories to your PATH (add to `~/.zshrc` for
persistence):

```bash
export PATH="/opt/homebrew/opt/coreutils/libexec/gnubin:/opt/homebrew/opt/gnu-tar/libexec/gnubin:$PATH"
```

### Running a focused test

```bash
make test-e2e-node DOCKER=true \
  TARGET_BUILD_ARCH=linux/arm64 \
  TEST_ARGS="--kubelet-flags=--fail-swap-on=false" \
  FOCUS="should be updated when static pod updated" \
  PARALLELISM=1
```

### Why the extra flags

| Flag | Reason |
| --- | --- |
| `TARGET_BUILD_ARCH=linux/arm64` | The default `linux/amd64` runs under QEMU emulation on Apple Silicon. QEMU does not support seccomp, so `RunPodSandbox` fails with `seccomp is not supported`. Building for `linux/arm64` runs the container natively on Docker Desktop's ARM64 VM. |
| `TEST_ARGS="--kubelet-flags=--fail-swap-on=false"` | Docker Desktop's Linux VM has swap enabled. Without this flag kubelet refuses to start with `running with swap on is not supported`. |

### Subsequent runs

After the first run, the image and binaries are cached. Use
`SKIP_IMAGE_BUILD=true` to skip the `docker build` step:

```bash
make test-e2e-node DOCKER=true \
  TARGET_BUILD_ARCH=linux/arm64 \
  TEST_ARGS="--kubelet-flags=--fail-swap-on=false" \
  SKIP_IMAGE_BUILD=true \
  FOCUS="your test name here" \
  PARALLELISM=1
```

## Persistent containerd state

Container images pulled by kubelet are kept in a named Docker volume
mounted at `/var/lib/containerd`. The volume name is scoped to the
runner image tag (`k8s-e2e-node-containerd-<sanitized IMAGE_TAG>`) so
concurrent runs with different `IMAGE_TAG`s don't clobber each other's
containerd BoltDB. Subsequent runs with the same `IMAGE_TAG` skip the
image pull. To wipe it:

```
docker volume ls --filter name=k8s-e2e-node-containerd-
docker volume rm <volume>
```

## Dropping into the container manually

For ad-hoc debugging:

```
docker run --rm -it --privileged --cgroupns=host \
  -v "$(pwd):/go/src/k8s.io/kubernetes:rw" \
  -v "$(pwd)/_output/local/go/bin:/usr/local/bin/k8s-bin:ro" \
  -v "k8s-e2e-node-containerd-k8s-e2e-node-runner_dev:/var/lib/containerd" \
  --entrypoint bash \
  k8s-e2e-node-runner:dev
```

Then run `/usr/local/bin/entrypoint.sh bash` inside the container to get
a shell after containerd is up.

## Limitations

### No systemd — `[Disruptive]` tests that restart kubelet will fail

The container image does not run systemd as PID 1. Kubelet is launched
directly as a child process by the `local` runner binary
(`test/e2e_node/services/kubelet.go`, line ~264, the non-systemd branch).

Many `[Disruptive]` and `[Serial]` tests call `restartKubelet()` or
`mustStopKubelet()` (defined in `test/e2e_node/util.go`, lines 413–445)
to stop and restart kubelet mid-test. These helpers unconditionally shell
out to `systemctl`:

- `findKubeletServiceName()` runs
  `sudo systemctl list-units *kubelet*` to discover the kubelet's
  transient unit name (`kubelet-<timestamp>.service`).
- `restartKubelet()` runs `sudo systemctl reset-failed` and
  `sudo systemctl restart`.
- `mustStopKubelet()` runs `sudo systemctl kill` and returns a closure
  that calls `sudo systemctl restart`.

Since systemd is not running inside the container, all of these fail
with `exit status 1` at the `systemctl` invocation. The test body itself
may execute successfully, but the `AfterEach` cleanup (which typically
calls `restartKubelet(ctx, true)`) fails, causing the test to be
reported as failed.

**Affected tests include** (non-exhaustive):

- `test/e2e_node/device_plugin_multiple_test.go` — all 3 tests
- `test/e2e_node/dra_test.go` — most DRA tests (12+ call sites)
- `test/e2e_node/node_container_manager_test.go`
- `test/e2e_node/standalone_test.go`
- `test/e2e_node/kubelet_config_dir_test.go`
- `test/e2e_node/restart_all_containers_test.go`
- Any test tagged `[Disruptive]` that manages kubelet lifecycle

**Workaround**: skip disruptive tests with
`SKIP="\[Disruptive\]"` (this is included in the default SKIP pattern).

**Possible future fixes**:

1. *Add non-systemd support to `util.go`*. The runner
   (`test/e2e_node/services/kubelet.go`, line 224) already has a
   two-branch structure: when `systemd-run` is found, kubelet is
   launched as a transient systemd unit with `killCommand` and
   `restartCommand` set to `systemctl kill/restart`; when systemd is
   absent, kubelet is launched as a direct child process. The problem is
   that `util.go`'s `restartKubelet()` and `mustStopKubelet()` bypass
   the runner entirely and hard-code `systemctl`. To fix this, the
   runner would need to persist the kubelet launch command (e.g., write
   it to `/run/kubelet-cmd.sh` or a PID file), and `util.go` would need
   a fallback path that kills the process by PID and re-launches from
   the saved command. The challenge is preserving the full command-line
   flags, log file redirection, and cgroup placement that the runner
   sets up. See also `TODO` at `util.go:321`:
   `// TODO: Find a uniform way to deal with systemctl/initctl/service operations. #34494`.

2. *Run systemd as PID 1 inside the container*. Install systemd in the
   Docker image, use `/sbin/init` as the entrypoint, and launch both
   containerd and the test runner as systemd services. This would make
   `systemd-run` available so the runner's systemd branch activates
   automatically, and all `systemctl` calls in `util.go` would work.
   This requires `--stop-signal=SIGRTMIN+3` for clean shutdown, masking
   unneeded units, and reworking the entrypoint flow. It is the most
   robust path but a significant refactor.

### No debug tool support

`E2E_TEST_DEBUG_TOOL=dlv` and `E2E_TEST_DEBUG_TOOL=gdb` are explicitly
rejected in `DOCKER=true` mode. The runner image does not ship `delve`
or `gdb`, and the `hack/make-rules/test-e2e-node.sh` dispatcher exits
with a clear error before reaching the container.

### Architecture limited to the build target

The runner image is built for a single `--platform` matching
`TARGET_BUILD_ARCH` (default `linux/amd64`). Multi-arch images are not
built. On Apple Silicon, you must pass `TARGET_BUILD_ARCH=linux/arm64`
(see the macOS section above); the default `linux/amd64` runs under
QEMU where seccomp is not supported, causing `RunPodSandbox` to fail.

### `REMOTE=true` is incompatible

`DOCKER=true` with `REMOTE=true` is explicitly rejected by the
dispatcher with a clear error. The Docker mode is a local-only flow.

## File map

- `Dockerfile` — Ubuntu 24.04 base + containerd/CNI/etcd/runc install.
- `Dockerfile.dockerignore` — keeps the build context tiny.
- `setup-containerd.sh` — writes `/etc/containerd/config.toml`, the CNI
  conflist (subnet `10.100.0.0/16` to match the kubelet config's
  `podCIDR: 10.100.0.0/24`), `/etc/crictl.yaml`, and enables cgroup v2
  subtree controllers.
- `wait-for-containerd.sh` — polls the containerd UNIX socket on startup.
- `entrypoint.sh` — cgroup pre-flight, starts containerd, hands off to
  the test command.

## Related

- `hack/run-e2e-node-container.sh` — host-side wrapper.
- `hack/make-rules/test-e2e-node.sh` — dispatch (`DOCKER=true` branch).
- cri-tools `hack/run-e2e-container.sh` /
  `images/containerd-local-test/` — the upstream pattern this is based on.
