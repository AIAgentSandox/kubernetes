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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/pkg/apis/scheduling"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/test/utils/ktesting"
)

const (
	systemPartitionNamespace = "kube-system"
	systemPartition          = "system"
)

// fakeNodeAdmitHandler is a stand-in for the node-wide eviction admit handler.
// It returns the configured result for every pod so the composite handler's
// node-wide delegation can be exercised in isolation.
type fakeNodeAdmitHandler struct {
	result lifecycle.PodAdmitResult
}

func (f *fakeNodeAdmitHandler) Admit(_ context.Context, _ *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	return f.result
}

// fakePartitionManager is a Manager whose pressure state is fixed, so the
// composite admit handler's partition checks can be driven deterministically.
type fakePartitionManager struct {
	memoryPressure bool
}

func (f *fakePartitionManager) Start(_ context.Context, _ DiskInfoProvider, _ ActivePodsFunc, _ PodCleanedUpFunc, _ time.Duration) {
}
func (f *fakePartitionManager) IsUnderMemoryPressure() bool { return f.memoryPressure }
func (f *fakePartitionManager) IsUnderDiskPressure() bool   { return false }
func (f *fakePartitionManager) IsUnderPIDPressure() bool    { return false }

// newAdmitPod builds a pod in the given namespace with the given priority.
func newAdmitPod(name, namespace string, priority int32) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(name),
		},
		Spec: v1.PodSpec{
			Priority: &priority,
		},
	}
}

// namespacePartitionFunc routes pods in the system partition namespace to the
// system partition and all other pods to the default partition.
func namespacePartitionFunc(pod *v1.Pod) string {
	if pod.Namespace == systemPartitionNamespace {
		return systemPartition
	}
	return ""
}

func TestCompositeEvictionAdmitHandlerNodePressureRejectsNonCriticalPods(t *testing.T) {
	tCtx := ktesting.Init(t)

	// Node-wide handler rejects the pod (simulating existing node pressure
	// behavior for a non-critical pod).
	nodeReject := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{
		Admit:   false,
		Reason:  Reason,
		Message: "node under pressure",
	}}
	handler := newCompositeEvictionAdmitHandler(
		nodeReject,
		map[string]Manager{systemPartition: &fakePartitionManager{memoryPressure: false}},
		namespacePartitionFunc,
	)

	// A non-critical default-partition pod must be rejected by the node-wide
	// decision, preserving existing behavior.
	pod := newAdmitPod("default-pod", "default", scheduling.DefaultPriorityWhenNoDefaultClassExists)
	result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: pod})
	if result.Admit {
		t.Errorf("expected node-wide pressure to reject non-critical pod, got admit")
	}
	if result.Message != "node under pressure" {
		t.Errorf("expected node-wide rejection message to be preserved, got %q", result.Message)
	}
}

func TestCompositeEvictionAdmitHandlerNodePressureAdmitsCriticalPods(t *testing.T) {
	tCtx := ktesting.Init(t)

	// When the node-wide handler admits a pod (existing behavior admits critical
	// pods), the composite handler must also admit it.
	nodeAdmit := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{Admit: true}}
	handler := newCompositeEvictionAdmitHandler(
		nodeAdmit,
		map[string]Manager{systemPartition: &fakePartitionManager{memoryPressure: false}},
		namespacePartitionFunc,
	)

	pod := newAdmitPod("critical-pod", "default", scheduling.SystemCriticalPriority)
	result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: pod})
	if !result.Admit {
		t.Errorf("expected critical pod to be admitted, got reject: %q", result.Message)
	}
}

func TestCompositeEvictionAdmitHandlerPartitionPressureRejectsOnlyPartitionPods(t *testing.T) {
	tCtx := ktesting.Init(t)

	nodeAdmit := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{Admit: true}}
	handler := newCompositeEvictionAdmitHandler(
		nodeAdmit,
		map[string]Manager{systemPartition: &fakePartitionManager{memoryPressure: true}},
		namespacePartitionFunc,
	)

	// A non-critical pod targeting the pressured system partition must be
	// rejected.
	systemPod := newAdmitPod("system-pod", systemPartitionNamespace, scheduling.DefaultPriorityWhenNoDefaultClassExists)
	if result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: systemPod}); result.Admit {
		t.Errorf("expected partition pressure to reject pod targeting the partition, got admit")
	} else if result.Reason != Reason {
		t.Errorf("expected partition rejection reason %q, got %q", Reason, result.Reason)
	}

	// A non-critical pod in the default partition is unaffected by the system
	// partition's pressure.
	defaultPod := newAdmitPod("default-pod", "default", scheduling.DefaultPriorityWhenNoDefaultClassExists)
	if result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: defaultPod}); !result.Admit {
		t.Errorf("expected default-partition pod to be admitted while only the system partition is under pressure, got reject: %q", result.Message)
	}
}

func TestCompositeEvictionAdmitHandlerNoPressureAdmitsAllPods(t *testing.T) {
	tCtx := ktesting.Init(t)

	nodeAdmit := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{Admit: true}}
	handler := newCompositeEvictionAdmitHandler(
		nodeAdmit,
		map[string]Manager{systemPartition: &fakePartitionManager{memoryPressure: false}},
		namespacePartitionFunc,
	)

	pods := []*v1.Pod{
		newAdmitPod("system-pod", systemPartitionNamespace, scheduling.DefaultPriorityWhenNoDefaultClassExists),
		newAdmitPod("default-pod", "default", scheduling.DefaultPriorityWhenNoDefaultClassExists),
	}
	for _, pod := range pods {
		if result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: pod}); !result.Admit {
			t.Errorf("expected pod %q to be admitted without pressure, got reject: %q", pod.Name, result.Message)
		}
	}
}

func TestCompositeEvictionAdmitHandlerCriticalPodsAlwaysAdmittedUnderPartitionPressure(t *testing.T) {
	tCtx := ktesting.Init(t)

	nodeAdmit := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{Admit: true}}
	handler := newCompositeEvictionAdmitHandler(
		nodeAdmit,
		map[string]Manager{systemPartition: &fakePartitionManager{memoryPressure: true}},
		namespacePartitionFunc,
	)

	// A critical pod targeting the pressured system partition must still be
	// admitted.
	criticalSystemPod := newAdmitPod("critical-system-pod", systemPartitionNamespace, scheduling.SystemCriticalPriority)
	if result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: criticalSystemPod}); !result.Admit {
		t.Errorf("expected critical pod to be admitted despite partition pressure, got reject: %q", result.Message)
	}
}

func TestCompositeEvictionAdmitHandlerUnknownPartitionAdmits(t *testing.T) {
	tCtx := ktesting.Init(t)

	// The pod routes to a partition that has no registered manager; it must be
	// admitted rather than panicking on a missing map entry.
	nodeAdmit := &fakeNodeAdmitHandler{result: lifecycle.PodAdmitResult{Admit: true}}
	handler := newCompositeEvictionAdmitHandler(
		nodeAdmit,
		map[string]Manager{},
		namespacePartitionFunc,
	)

	systemPod := newAdmitPod("system-pod", systemPartitionNamespace, scheduling.DefaultPriorityWhenNoDefaultClassExists)
	if result := handler.Admit(tCtx, &lifecycle.PodAdmitAttributes{Pod: systemPod}); !result.Admit {
		t.Errorf("expected pod targeting a partition with no manager to be admitted, got reject: %q", result.Message)
	}
}
