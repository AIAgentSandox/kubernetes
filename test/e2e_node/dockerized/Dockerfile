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

# Ubuntu 24.04 base image for running node-e2e tests inside a privileged
# container. Modeled on cri-tools' images/containerd-local-test/Dockerfile;
# kept at ubuntu:24.04 (cri-tools currently uses ubuntu:26.04) per the
# task description so we stay on a current LTS release.
FROM ubuntu:24.04

# Install runtime + node-e2e dependencies. This is the cri-tools superset
# plus packages the kubelet/node-e2e flow reaches for (conntrack, ebtables,
# ethtool, iproute2, jq, kmod, runc).
RUN apt-get update && apt-get install -y \
    apparmor-utils \
    ca-certificates \
    conntrack \
    containerd \
    curl \
    ebtables \
    ethtool \
    iproute2 \
    iptables \
    jq \
    kmod \
    libseccomp2 \
    runc \
    socat \
    sudo \
    && rm -rf /var/lib/apt/lists/*

# Install CNI plugins (bridge/portmap/firewall/tuning are required by the
# conflist written by setup-containerd.sh).
ARG CNI_PLUGINS_VERSION=v1.4.0
RUN mkdir -p /opt/cni/bin && \
    curl -L "https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-$(dpkg --print-architecture)-${CNI_PLUGINS_VERSION}.tgz" | \
    tar -C /opt/cni/bin -xz

# Install etcd. Keep ETCD_VERSION in sync with build/dependencies.yaml
# (authoritative source: hack/lib/etcd.sh::kube::etcd::install). The
# embedded test etcd already comes from the e2e_node test binary, so this
# is only here for manual debugging inside the container.
ARG ETCD_VERSION=3.6.11
RUN ARCH="$(dpkg --print-architecture)" && \
    curl -L "https://github.com/etcd-io/etcd/releases/download/v${ETCD_VERSION}/etcd-v${ETCD_VERSION}-linux-${ARCH}.tar.gz" -o /tmp/etcd.tar.gz && \
    tar -xzf /tmp/etcd.tar.gz -C /tmp && \
    mv "/tmp/etcd-v${ETCD_VERSION}-linux-${ARCH}/etcd" /usr/local/bin/etcd && \
    mv "/tmp/etcd-v${ETCD_VERSION}-linux-${ARCH}/etcdctl" /usr/local/bin/etcdctl && \
    rm -rf /tmp/etcd.tar.gz "/tmp/etcd-v${ETCD_VERSION}-linux-${ARCH}"

# Copy containerd setup + wait helpers and the entrypoint. These scripts
# are added by Task 3 and Task 4 in this plan; the Dockerfile references
# them so a manual build succeeds once those files land.
COPY test/e2e_node/dockerized/setup-containerd.sh /usr/local/bin/setup-containerd.sh
COPY test/e2e_node/dockerized/wait-for-containerd.sh /usr/local/bin/wait-for-containerd.sh
COPY test/e2e_node/dockerized/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/setup-containerd.sh /usr/local/bin/wait-for-containerd.sh /usr/local/bin/entrypoint.sh

# Host-built binaries (kubelet, e2e_node.test, ginkgo, mounter,
# gcp-credential-provider) are bind-mounted at /usr/local/bin/k8s-bin by
# hack/run-e2e-node-container.sh. Putting it first on PATH mirrors
# cri-tools' /usr/local/bin/critest-tools convention.
ENV PATH="/usr/local/bin/k8s-bin:${PATH}"

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
