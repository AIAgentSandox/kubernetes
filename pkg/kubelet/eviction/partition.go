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
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

// systemPartitionName is the value of the "partition" metric label used for the
// node system partition.
const systemPartitionName = "system"

// partitionMemoryReader reads the current memory usage (in bytes) of the cgroup
// located at the given cgroupfs path (relative to the memory cgroup mount
// point). It is defined per-platform so that non-Linux builds compile.
type partitionMemoryReader func(cgroupPath string) (uint64, error)

// partitionMemoryAvailable computes the number of bytes available in a partition
// given the configured memory limit and the current usage. It never returns a
// negative value.
func partitionMemoryAvailable(memoryLimit *resource.Quantity, usage uint64) *resource.Quantity {
	available := max(memoryLimit.Value()-int64(usage), 0)
	return resource.NewQuantity(available, resource.BinarySI)
}

// partitionMemoryThresholdMet reports whether the partition memory eviction
// threshold has been crossed. The threshold value is interpreted relative to the
// partition memory limit: a percentage threshold is applied against memoryLimit,
// and an absolute quantity is compared directly. The threshold is met when the
// available memory is strictly less than the resolved threshold quantity, which
// matches the OpLessThan semantics used for the node-level memory.available
// signal.
func partitionMemoryThresholdMet(threshold evictionapi.ThresholdValue, memoryLimit *resource.Quantity, usage uint64) bool {
	available := partitionMemoryAvailable(memoryLimit, usage)
	thresholdQuantity := evictionapi.GetThresholdQuantity(threshold, memoryLimit)
	return available.Cmp(*thresholdQuantity) < 0
}

// memoryAvailableHardThreshold returns the hard memory.available eviction
// threshold from the provided set, if one is configured. The partition reuses
// the node-level hard threshold to decide when to evict system-partition pods.
func memoryAvailableHardThreshold(thresholds []evictionapi.Threshold) (evictionapi.Threshold, bool) {
	for _, threshold := range thresholds {
		if threshold.Signal == evictionapi.SignalMemoryAvailable && isHardEvictionThreshold(threshold) {
			return threshold, true
		}
	}
	return evictionapi.Threshold{}, false
}
