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
	"context"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/pkg/kubelet/server/stats"
	"k8s.io/utils/clock"
)

// SystemPartitionName is the canonical name of the node system partition. It is
// used as the map key for the partition's eviction manager and as the value of
// the "partition" metric label.
const SystemPartitionName = "system"

// PartitionThresholds returns the subset of the provided node-level thresholds
// that a partition eviction manager can evaluate — currently the hard
// memory.available threshold. A partition manager only reads memory stats, so
// disk, PID, and soft thresholds do not apply; reusing the node hard threshold
// preserves the pre-refactor partition eviction trigger. The result is nil when
// no hard memory.available threshold is configured.
func PartitionThresholds(thresholds []evictionapi.Threshold) []evictionapi.Threshold {
	threshold, found := memoryAvailableHardThreshold(thresholds)
	if !found {
		return nil
	}
	return []evictionapi.Threshold{threshold}
}

// partitionMemoryReader reads the current memory usage (in bytes) of the cgroup
// located at the given cgroupfs path (relative to the memory cgroup mount
// point). It is defined per-platform so that non-Linux builds compile.
type partitionMemoryReader func(cgroupPath string) (uint64, error)

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

// NewCgroupPartitionStatsProvider returns a PartitionStatsProvider that reads the
// partition memory usage from the cgroup filesystem at cgroupPath and reports the
// configured memoryLimit. It is the entry point used by callers outside this
// package (e.g. the kubelet) to build the stats source for a partition manager.
func NewCgroupPartitionStatsProvider(cgroupPath string, memoryLimit *resource.Quantity) PartitionStatsProvider {
	return newCgroupPartitionStatsProvider(cgroupPath, memoryLimit)
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

// partitionSummaryProvider adapts a PartitionStatsProvider into the
// stats.SummaryProvider interface expected by the eviction control loop. It
// produces a minimal Summary that carries only the partition's memory usage,
// shaped so that makeSignalObservations derives a memory.available signal whose
// available value is (limit - usage) and whose capacity is the partition limit.
// This lets a partition-scoped managerImpl reuse the node-level eviction logic
// unchanged.
type partitionSummaryProvider struct {
	// statsProvider reads the partition's current memory usage and limit.
	statsProvider PartitionStatsProvider
	// clock timestamps each observation so the eviction loop can detect updated
	// stats between synchronize cycles.
	clock clock.Clock
}

var _ stats.SummaryProvider = &partitionSummaryProvider{}

// newPartitionSummaryProvider returns a SummaryProvider backed by the given
// partition stats provider.
func newPartitionSummaryProvider(statsProvider PartitionStatsProvider, clock clock.Clock) *partitionSummaryProvider {
	return &partitionSummaryProvider{
		statsProvider: statsProvider,
		clock:         clock,
	}
}

// Get returns a Summary populated with the partition's memory stats. The
// available bytes are computed as limit minus usage (never negative), and the
// working set bytes carry the current usage, so that available+workingSet equals
// the partition limit — matching the semantics the eviction logic expects.
func (p *partitionSummaryProvider) Get(ctx context.Context, updateStats bool) (*statsapi.Summary, error) {
	partitionStats, err := p.statsProvider.GetPartitionStats()
	if err != nil {
		return nil, err
	}
	workingSetBytes := partitionStats.MemoryUsageBytes
	var availableBytes uint64
	if partitionStats.MemoryLimitBytes > partitionStats.MemoryUsageBytes {
		availableBytes = partitionStats.MemoryLimitBytes - partitionStats.MemoryUsageBytes
	}
	now := metav1.NewTime(p.clock.Now())
	return &statsapi.Summary{
		Node: statsapi.NodeStats{
			Memory: &statsapi.MemoryStats{
				Time:            now,
				AvailableBytes:  &availableBytes,
				WorkingSetBytes: &workingSetBytes,
			},
		},
	}, nil
}

// GetCPUAndMemoryStats returns the same partition memory Summary as Get. The
// partition manager only evaluates memory signals.
func (p *partitionSummaryProvider) GetCPUAndMemoryStats(ctx context.Context) (*statsapi.Summary, error) {
	return p.Get(ctx, false)
}
