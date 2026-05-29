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

# Entrypoint for the node-e2e runner image.
#
# Responsibilities:
#   1. Prepare cgroup v2 so kubelet (and containerd) can create child
#      cgroups under the privileged container's root.
#   2. Write the containerd + CNI config and start containerd in the
#      background, waiting for the socket before continuing.
#   3. If KUBELET_CONFIG_FILE is provided as a path relative to the
#      bind-mounted source tree, make it discoverable to run_local.go.
#   4. cd into the bind-mounted Kubernetes source root and exec the
#      caller-supplied command (typically the e2e_node runner).
#   5. On exit, kill containerd and propagate the test command's exit code.
#
# Modeled after cri-tools' images/containerd-local-test/entrypoint.sh.

set -euo pipefail

# In cgroup v2, the root cgroup cannot host processes if we want to write
# cgroup.subtree_control. Move our shell into an init sub-cgroup before
# setup-containerd.sh tries to enable subtree controllers on /.
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
    mkdir -p /sys/fs/cgroup/init
    echo 0 > /sys/fs/cgroup/init/cgroup.procs
fi

echo "Initializing container environment..."
setup-containerd.sh

mkdir -p /run/containerd /var/lib/kubelet /var/log /var/result

echo "Starting containerd..."
containerd > /var/log/containerd.log 2>&1 &
CONTAINERD_PID=$!

# shellcheck disable=SC2317  # invoked via trap, not directly.
cleanup() {
    if [ -n "${CONTAINERD_PID:-}" ] && kill -0 "${CONTAINERD_PID}" 2>/dev/null; then
        echo "Cleaning up containerd (PID: ${CONTAINERD_PID})..."
        kill "${CONTAINERD_PID}" || true
        wait "${CONTAINERD_PID}" 2>/dev/null || true
    fi
}
trap cleanup EXIT

wait-for-containerd.sh

# If the caller pointed KUBELET_CONFIG_FILE at a host path that does not
# exist inside the bind-mounted source tree, surface a clear error early
# rather than letting run_local.go fail mid-test.
if [ -n "${KUBELET_CONFIG_FILE:-}" ] && [ ! -e "${KUBELET_CONFIG_FILE}" ]; then
    echo "ERROR: KUBELET_CONFIG_FILE=${KUBELET_CONFIG_FILE} does not exist inside the container." >&2
    echo "       It must be a path under the bind-mounted source tree." >&2
    exit 1
fi

cd /go/src/k8s.io/kubernetes
echo "Executing: $*"
# Run (don't exec) so the EXIT trap above can stop containerd and we can
# propagate the test command's exit code. Tests routinely exit non-zero;
# disable set -e for this call so cleanup runs deterministically.
set +e
"$@"
EXIT_CODE=$?
set -e
exit "${EXIT_CODE}"
