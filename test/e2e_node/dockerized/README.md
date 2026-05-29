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
| `PARALLELISM` | unset → ginkgo default (`cores - 1`) | Forwarded as ginkgo `-nodes` when set to a value > 1. |
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

## Persistent containerd state

Container images pulled by kubelet are kept in the named Docker volume
`k8s-e2e-node-containerd` (mounted at `/var/lib/containerd`). Subsequent
runs skip the image pull. To wipe it:

```
docker volume rm k8s-e2e-node-containerd
```

## Dropping into the container manually

For ad-hoc debugging:

```
docker run --rm -it --privileged --cgroupns=host \
  -v "$(pwd):/go/src/k8s.io/kubernetes:rw" \
  -v "$(pwd)/_output/local/go/bin:/usr/local/bin/k8s-bin:ro" \
  -v "k8s-e2e-node-containerd:/var/lib/containerd" \
  --entrypoint bash \
  k8s-e2e-node-runner:dev
```

Then run `/usr/local/bin/entrypoint.sh bash` inside the container to get
a shell after containerd is up.

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
