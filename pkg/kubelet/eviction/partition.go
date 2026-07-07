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
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

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

// filterPodsByNamespaces returns the subset of pods whose namespace belongs to
// the provided set. It is used to restrict system-partition eviction candidates
// to pods that actually run in the partition. An empty namespace set yields no
// pods.
func filterPodsByNamespaces(pods []*v1.Pod, namespaces sets.Set[string]) []*v1.Pod {
	if namespaces.Len() == 0 {
		return nil
	}
	filtered := make([]*v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if namespaces.Has(pod.Namespace) {
			filtered = append(filtered, pod)
		}
	}
	return filtered
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
