#!/usr/bin/env bash

# Copyright The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Run the node-e2e local flow inside a privileged container.
#
# This is the implementation behind `make test-e2e-node DOCKER=true`. The
# Makefile / hack/make-rules/test-e2e-node.sh dispatcher exports FOCUS,
# SKIP, LABEL_FILTER, TEST_ARGS, PARALLELISM, ARTIFACTS, KUBELET_CONFIG_FILE,
# RUNTIME_CONFIG, EXTRA_ENVS, SYSTEM_SPEC_NAME, TIMEOUT, RUN_UNTIL_FAILURE,
# IMAGE_TAG and CONTAINER_RUNTIME_ENDPOINT before exec'ing this script. We
# rebuild the ginkgo flag string from those inputs because the upstream
# script keeps it in a local variable.
#
# Modeled on cri-tools' hack/run-e2e-container.sh.

set -o errexit
set -o nounset
set -o pipefail

KUBE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${KUBE_ROOT}/hack/lib/init.sh"

kube::golang::setup_env

# --- Inputs -----------------------------------------------------------------
#
# Only inputs that this script itself interprets (image tag, build skip,
# dockerized-build override, target arch) need defaults; everything else is
# already defaulted by hack/make-rules/test-e2e-node.sh before we are invoked.
image_tag="${IMAGE_TAG:-k8s-e2e-node-runner:dev}"
skip_image_build="${SKIP_IMAGE_BUILD:-false}"
# Only linux/amd64 is supported in the first iteration (see plan Q4); we still
# accept TARGET_BUILD_ARCH so dockerized builds can target a different arch
# later without a code change.
target_arch="${TARGET_BUILD_ARCH:-linux/amd64}"

artifacts="${ARTIFACTS:?ARTIFACTS must be set (hack/make-rules/test-e2e-node.sh exports it)}"
mkdir -p "${artifacts}"

# --- Build mode selection ---------------------------------------------------
#
# - Linux + host arch matches the target arch: reuse _output/local/go/bin.
# - Otherwise (or USE_DOCKERIZED_BUILD=true): use the dockerized cross-build
#   so we always end up with linux/amd64 binaries that match the runner
#   image's userland.
host_os="$(uname -s | tr '[:upper:]' '[:lower:]')"
host_arch_uname="$(uname -m)"
case "${host_arch_uname}" in
  x86_64|amd64) host_arch="amd64" ;;
  aarch64|arm64) host_arch="arm64" ;;
  *) host_arch="${host_arch_uname}" ;;
esac

target_arch_only="${target_arch#*/}"
use_dockerized_build="${USE_DOCKERIZED_BUILD:-false}"
if [[ "${host_os}" != "linux" || "${host_arch}" != "${target_arch_only}" ]]; then
  use_dockerized_build="true"
fi

if [[ "${use_dockerized_build}" == "true" ]]; then
  bin_dir="${KUBE_ROOT}/_output/dockerized/bin/${target_arch}"
else
  bin_dir="${KUBE_ROOT}/_output/local/go/bin"
fi

# Targets we need inside the container. `local` is the runner binary built
# from test/e2e_node/runner/local; we invoke it via its absolute path inside
# the container so the bash builtin of the same name never gets in the way.
required_bins=(
  e2e_node.test
  ginkgo
  kubelet
  mounter
  gcp-credential-provider
  local
)
build_targets=(
  cmd/kubelet
  test/e2e_node/e2e_node.test
  github.com/onsi/ginkgo/v2/ginkgo
  cluster/gce/gci/mounter
  test/e2e_node/plugins/gcp-credential-provider
  test/e2e_node/runner/local
)

need_build="false"
for bin in "${required_bins[@]}"; do
  if [[ ! -x "${bin_dir}/${bin}" ]]; then
    need_build="true"
    break
  fi
done

if [[ "${need_build}" == "true" ]]; then
  kube::log::status "Building node-e2e binaries (${target_arch}, dockerized=${use_dockerized_build})"
  if [[ "${use_dockerized_build}" == "true" ]]; then
    KUBE_BUILD_PLATFORMS="${target_arch}" \
      "${KUBE_ROOT}/build/run.sh" make WHAT="${build_targets[*]}"
  else
    make -C "${KUBE_ROOT}" WHAT="${build_targets[*]}" KUBE_BUILD_PLATFORMS="${target_arch}"
  fi
else
  kube::log::status "Reusing prebuilt node-e2e binaries from ${bin_dir}"
fi

# Sanity check.
for bin in "${required_bins[@]}"; do
  if [[ ! -x "${bin_dir}/${bin}" ]]; then
    kube::log::error "Required binary ${bin_dir}/${bin} is missing after build"
    exit 1
  fi
done

# --- Image build ------------------------------------------------------------
# BuildKit is required so the per-Dockerfile `Dockerfile.dockerignore` is
# honored; on a legacy builder the entire repo would be sent as build context.
# --platform is passed so the image userland matches the binaries we just
# built — without it `docker build` defaults to the host arch, which would
# silently produce an amd64 image with arm64 binaries when a user sets
# TARGET_BUILD_ARCH=linux/arm64 on an amd64 host.
export DOCKER_BUILDKIT=1
if [[ "${skip_image_build}" == "true" ]]; then
  kube::log::status "SKIP_IMAGE_BUILD=true; using existing image ${image_tag}"
else
  kube::log::status "Building runner image ${image_tag} (platform=${target_arch})"
  docker build \
    --platform "${target_arch}" \
    -t "${image_tag}" \
    -f "${KUBE_ROOT}/test/e2e_node/dockerized/Dockerfile" \
    "${KUBE_ROOT}"
fi

# --- Compose ginkgo flags ---------------------------------------------------
focus="${FOCUS:-}"
label_filter="${LABEL_FILTER:-}"
if [[ -n "${label_filter}" ]]; then
  skip="${SKIP:-}"
else
  skip="${SKIP-"\[Flaky\]|\[Slow\]|\[Serial\]"}"
fi

# Honor TIMEOUT if the caller set it; otherwise keep the historical 24h
# ceiling used by the local/remote paths.
ginkgoflags="-timeout=${TIMEOUT:-24h}"
# The dispatcher resolves PARALLELISM (default 8) before exec'ing this
# script, so we just forward whatever it set. -nodes is only meaningful
# above 1.
if [[ -n "${PARALLELISM:-}" && "${PARALLELISM}" -gt 1 ]]; then
  ginkgoflags="${ginkgoflags} -nodes=${PARALLELISM} "
fi
if [[ -n "${focus}" ]]; then
  ginkgoflags="${ginkgoflags} -focus=\"${focus}\" "
fi
if [[ -n "${skip}" ]]; then
  ginkgoflags="${ginkgoflags} -skip=\"${skip}\" "
fi
if [[ -n "${label_filter}" ]]; then
  ginkgoflags="${ginkgoflags} --label-filter=\"${label_filter}\" "
fi
if [[ "${RUN_UNTIL_FAILURE:-false}" == "true" ]]; then
  ginkgoflags="${ginkgoflags} --until-it-fails=true "
fi

# --- Optional host bind-mounts ----------------------------------------------
optional_mounts=()
if [[ -d /lib/modules ]]; then
  optional_mounts+=("-v" "/lib/modules:/lib/modules:ro")
fi
if [[ -d /etc/apparmor.d ]]; then
  optional_mounts+=("-v" "/etc/apparmor.d:/etc/apparmor.d:ro")
fi

# Use a deterministic container hostname so the runner can pass --node-name
# to e2e_node.test that matches kubelet's reported hostname inside the
# container.
container_hostname="e2e-node-runner"

# --- Docker run -------------------------------------------------------------
#
# The container's bash receives the inner command verbatim (single-quoted on
# the host side) so it can expand env vars and $(hostname) at runtime. Host
# values are propagated via -e and re-quoted by the container's bash, so
# embedded regex characters in FOCUS/SKIP survive both shells.
# shellcheck disable=SC2016 # the inner expansions intentionally happen in the
# container's shell at runtime, not on the host.
inner_cmd='/usr/local/bin/k8s-bin/local \
  --build-dependencies=false \
  --k8s-bin-dir=/usr/local/bin/k8s-bin \
  --use-dockerized-build="${E2E_USE_DOCKERIZED_BUILD}" \
  --target-build-arch="${E2E_TARGET_BUILD_ARCH}" \
  --ginkgo-flags="${E2E_GINKGO_FLAGS}" \
  --test-flags="--v 4 --report-dir=/var/result --node-name $(hostname) ${E2E_TEST_ARGS}" \
  --kubelet-config-file="${E2E_KUBELET_CONFIG_FILE}" \
  --runtime-config="${E2E_RUNTIME_CONFIG}" \
  --extra-envs="${E2E_EXTRA_ENVS}" \
  --system-spec-name="${E2E_SYSTEM_SPEC_NAME}"'

kube::log::status "Starting e2e-node container (image=${image_tag}, artifacts=${artifacts})"
docker run --rm --privileged \
  --platform "${target_arch}" \
  --cgroupns=host \
  --hostname "${container_hostname}" \
  -e KUBE_ROOT=/go/src/k8s.io/kubernetes \
  -e CONTAINER_RUNTIME_ENDPOINT="${CONTAINER_RUNTIME_ENDPOINT:-unix:///run/containerd/containerd.sock}" \
  -e ARTIFACTS=/var/result \
  -e KUBELET_CONFIG_FILE="${KUBELET_CONFIG_FILE}" \
  -e E2E_USE_DOCKERIZED_BUILD="${use_dockerized_build}" \
  -e E2E_TARGET_BUILD_ARCH="${target_arch}" \
  -e E2E_GINKGO_FLAGS="${ginkgoflags}" \
  -e E2E_TEST_ARGS="${TEST_ARGS:-}" \
  -e E2E_KUBELET_CONFIG_FILE="${KUBELET_CONFIG_FILE}" \
  -e E2E_RUNTIME_CONFIG="${RUNTIME_CONFIG:-}" \
  -e E2E_EXTRA_ENVS="${EXTRA_ENVS:-}" \
  -e E2E_SYSTEM_SPEC_NAME="${SYSTEM_SPEC_NAME:-}" \
  -v "${KUBE_ROOT}:/go/src/k8s.io/kubernetes:rw" \
  -v "${bin_dir}:/usr/local/bin/k8s-bin:ro" \
  -v "k8s-e2e-node-containerd:/var/lib/containerd" \
  -v "${artifacts}:/var/result" \
  "${optional_mounts[@]}" \
  "${image_tag}" \
  bash -c "${inner_cmd}" 2>&1 | tee -i "${artifacts}/build-log.txt"

# `pipefail` already propagates a docker-run failure; this explicit exit
# only reasserts the docker exit code (PIPESTATUS[0]) on the success path
# for symmetry with the local/remote branches in test-e2e-node.sh.
exit "${PIPESTATUS[0]}"
