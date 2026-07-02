//go:build linux

/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package eviction

import (
	"fmt"

	libcontainercgroups "github.com/opencontainers/cgroups"
	"github.com/opencontainers/cgroups/fscommon"

	"k8s.io/kubernetes/pkg/kubelet/cm"
)

const (
	// cgroupV2MemoryUsageFile is the cgroup v2 file reporting current memory usage.
	cgroupV2MemoryUsageFile = "memory.current"
	// cgroupV1MemoryUsageFile is the cgroup v1 file reporting current memory usage.
	cgroupV1MemoryUsageFile = "memory.usage_in_bytes"
)

// readPartitionMemoryUsage reads the current memory usage (in bytes) of the
// cgroup at the given cgroupfs path (relative to the memory cgroup mount point,
// e.g. "/kubepods/system"). It supports both cgroup v1 and v2.
func readPartitionMemoryUsage(cgroupPath string) (uint64, error) {
	cgroups, err := cm.GetCgroupSubsystems()
	if err != nil {
		return 0, err
	}
	mountPoint, found := cgroups.MountPoints["memory"]
	if !found || len(mountPoint) == 0 {
		return 0, fmt.Errorf("memory cgroup mount point not found")
	}
	usageFile := cgroupV1MemoryUsageFile
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		usageFile = cgroupV2MemoryUsageFile
	}
	return fscommon.GetCgroupParamUint(mountPoint+cgroupPath, usageFile)
}
