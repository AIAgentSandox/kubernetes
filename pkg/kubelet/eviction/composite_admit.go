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
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	kubelettypes "k8s.io/kubernetes/pkg/kubelet/types"
)

// partitionConditionMessageFmt is the message reported when a pod is rejected
// because the partition it targets is under memory pressure.
const partitionConditionMessageFmt = "The pod's target partition %q was under memory pressure. "

// compositeEvictionAdmitHandler dispatches pod admission decisions across the
// node-wide eviction manager and the per-partition eviction managers. The
// node-wide admission decision preserves the existing behavior (critical pods
// are always admitted, best-effort pods are rejected under memory pressure,
// etc.). In addition, a pod that targets a partition is rejected when that
// partition's eviction manager reports memory pressure.
type compositeEvictionAdmitHandler struct {
	// nodeAdmitHandler makes the node-wide admission decision. It is the admit
	// handler returned alongside the node-wide eviction Manager, so node-level
	// admission behavior is unchanged. The plan referred to this as
	// "nodeManager Manager"; because the Manager interface does not expose Admit
	// and preserving existing node behavior requires the full admit logic, the
	// composite holds the node admit handler directly.
	nodeAdmitHandler lifecycle.PodAdmitHandler
	// partitionManagers holds the eviction manager for each configured partition,
	// keyed by partition name (e.g. "system"). Their pressure state gates
	// admission of pods that target the corresponding partition.
	partitionManagers map[string]Manager
	// podPartitionFunc maps a pod to the name of the partition it targets, or the
	// empty string for pods that run in the default (node-wide) partition.
	podPartitionFunc func(pod *v1.Pod) string
}

// ensure it implements the required interface
var _ lifecycle.PodAdmitHandler = &compositeEvictionAdmitHandler{}

// newCompositeEvictionAdmitHandler returns a PodAdmitHandler that combines the
// node-wide admission decision with per-partition memory pressure checks.
func newCompositeEvictionAdmitHandler(
	nodeAdmitHandler lifecycle.PodAdmitHandler,
	partitionManagers map[string]Manager,
	podPartitionFunc func(pod *v1.Pod) string,
) *compositeEvictionAdmitHandler {
	return &compositeEvictionAdmitHandler{
		nodeAdmitHandler:  nodeAdmitHandler,
		partitionManagers: partitionManagers,
		podPartitionFunc:  podPartitionFunc,
	}
}

// Admit evaluates whether a pod can be admitted. It first applies the node-wide
// admission decision (existing behavior). If the pod is admitted node-wide, it
// then rejects the pod when the partition it targets is under memory pressure.
// Critical pods are always admitted regardless of partition pressure.
func (c *compositeEvictionAdmitHandler) Admit(ctx context.Context, attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	// Node-wide admission first — this preserves existing behavior for node
	// conditions (memory/disk/PID pressure). A node-level rejection wins.
	if result := c.nodeAdmitHandler.Admit(ctx, attrs); !result.Admit {
		return result
	}

	// Critical pods are required for system stability and are always admitted,
	// even when their target partition is under pressure.
	if kubelettypes.IsCriticalPod(attrs.Pod) {
		return lifecycle.PodAdmitResult{Admit: true}
	}

	// Determine the partition the pod targets. Pods in the default (node-wide)
	// partition are not subject to any partition pressure check.
	partitionName := c.podPartitionFunc(attrs.Pod)
	if partitionName == "" {
		return lifecycle.PodAdmitResult{Admit: true}
	}

	manager, ok := c.partitionManagers[partitionName]
	if !ok {
		return lifecycle.PodAdmitResult{Admit: true}
	}

	if manager.IsUnderMemoryPressure() {
		return lifecycle.PodAdmitResult{
			Admit:   false,
			Reason:  Reason,
			Message: fmt.Sprintf(partitionConditionMessageFmt, partitionName),
		}
	}

	return lifecycle.PodAdmitResult{Admit: true}
}
