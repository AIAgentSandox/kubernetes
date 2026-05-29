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

# Writes the containerd config + CNI conflist used by the node-e2e runner
# image, and prepares the cgroup v2 root so kubelet can create pod cgroups
# from inside a privileged container.
#
# Adapted from cri-tools' hack/setup-containerd.sh. The two intentional
# differences from cri-tools:
#   * SystemdCgroup is forced false to match cgroupDriver: cgroupfs in
#     test/e2e_node/jenkins/default-kubelet-config.yaml.
#   * The CNI bridge subnet is 10.100.0.0/16 so it aligns with the
#     podCIDR: 10.100.0.0/24 already pinned in that same kubelet config
#     (cri-tools uses 10.88.0.0/16).

set -euo pipefail

CONTD_CONFIG_DIR="${CONTD_CONFIG_DIR:-/etc/containerd}"
CNI_CONFIG_DIR="${CNI_CONFIG_DIR:-/etc/cni/net.d}"
RUNTIME="${RUNTIME:-io.containerd.runc.v2}"

echo "Setting up containerd configuration in ${CONTD_CONFIG_DIR}..."
mkdir -p "${CONTD_CONFIG_DIR}"
cat <<EOF > "${CONTD_CONFIG_DIR}/config.toml"
version = 2
[plugins]
  [plugins."io.containerd.grpc.v1.cri"]
    [plugins."io.containerd.grpc.v1.cri".containerd]
      default_runtime_name = "runc"
      [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc]
        runtime_type = "${RUNTIME}"
        [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runc.options]
          SystemdCgroup = false
EOF

echo "Setting up CNI networking in ${CNI_CONFIG_DIR}..."
mkdir -p "${CNI_CONFIG_DIR}"
cat <<EOF > "${CNI_CONFIG_DIR}/10-containerd-net.conflist"
{
  "cniVersion": "1.0.0",
  "name": "containerd-net",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cni0",
      "isGateway": true,
      "ipMasq": true,
      "promiscMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [
          [{ "subnet": "10.100.0.0/16" }]
        ],
        "routes": [{ "dst": "0.0.0.0/0" }]
      }
    },
    {
      "type": "portmap",
      "capabilities": { "portMappings": true }
    },
    { "type": "firewall" },
    { "type": "tuning" }
  ]
}
EOF

echo "Containerd and CNI setup complete."

# Suppress crictl warnings about a missing config file.
touch /etc/crictl.yaml

# Enable cgroup v2 subtree controllers so kubelet can place pod cgroups
# under the root. Without this, --privileged containers cannot create
# child cgroups on a v2-unified host.
enable_subtree_controllers() {
    local controllers_file="$1"
    local subtree_file="$2"
    local controller
    tr ' ' '\n' < "${controllers_file}" | while read -r controller; do
        [ -z "${controller}" ] && continue
        echo "+${controller}" > "${subtree_file}" || true
    done
}

if [ -w /sys/fs/cgroup ]; then
    if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
        enable_subtree_controllers \
            /sys/fs/cgroup/cgroup.controllers \
            /sys/fs/cgroup/cgroup.subtree_control
        mkdir -p /sys/fs/cgroup/k8s.io
        if [ -f /sys/fs/cgroup/k8s.io/cgroup.controllers ]; then
            enable_subtree_controllers \
                /sys/fs/cgroup/k8s.io/cgroup.controllers \
                /sys/fs/cgroup/k8s.io/cgroup.subtree_control
        fi
    fi
fi
