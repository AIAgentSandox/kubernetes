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
	"k8s.io/apimachinery/pkg/api/resource"
)

// PartitionStats holds the resource usage stats for a single partition.
type PartitionStats struct {
	// MemoryUsageBytes is the current memory usage of the partition in bytes.
	MemoryUsageBytes uint64
	// MemoryLimitBytes is the configured memory limit of the partition in bytes.
	MemoryLimitBytes uint64
}

// PartitionStatsProvider provides resource usage stats for a single partition.
type PartitionStatsProvider interface {
	// GetPartitionStats returns the current resource usage of the partition.
	GetPartitionStats() (*PartitionStats, error)
}

// cgroupPartitionStatsProvider implements PartitionStatsProvider by reading the
// partition's current memory usage from the cgroup filesystem and reporting the
// configured memory limit.
type cgroupPartitionStatsProvider struct {
	// cgroupPath is the cgroupfs path of the partition cgroup relative to the
	// memory cgroup mount point (e.g. "/kubepods/system"). It is used to read the
	// current memory usage of the partition.
	cgroupPath string
	// memoryLimit is the configured memory limit of the partition.
	memoryLimit *resource.Quantity
	// readMemoryUsage reads the current memory usage (in bytes) of the partition
	// cgroup. It is a field so tests can substitute a fake reader.
	readMemoryUsage partitionMemoryReader
}

// newCgroupPartitionStatsProvider returns a PartitionStatsProvider that reads the
// partition memory usage from the cgroup filesystem and reports the configured
// memory limit.
func newCgroupPartitionStatsProvider(cgroupPath string, memoryLimit *resource.Quantity) *cgroupPartitionStatsProvider {
	return &cgroupPartitionStatsProvider{
		cgroupPath:      cgroupPath,
		memoryLimit:     memoryLimit,
		readMemoryUsage: readPartitionMemoryUsage,
	}
}

// GetPartitionStats reads the partition's current memory usage from the cgroup
// filesystem and returns it alongside the configured memory limit. It returns an
// error when the cgroup usage cannot be read.
func (p *cgroupPartitionStatsProvider) GetPartitionStats() (*PartitionStats, error) {
	usage, err := p.readMemoryUsage(p.cgroupPath)
	if err != nil {
		return nil, err
	}
	var limitBytes uint64
	if p.memoryLimit != nil {
		if value := p.memoryLimit.Value(); value > 0 {
			limitBytes = uint64(value)
		}
	}
	return &PartitionStats{
		MemoryUsageBytes: usage,
		MemoryLimitBytes: limitBytes,
	}, nil
}
