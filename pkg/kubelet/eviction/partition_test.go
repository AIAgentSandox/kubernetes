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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/klog/v2/ktesting"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	testingclock "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

func TestPartitionMemoryAvailable(t *testing.T) {
	limit := resource.NewQuantity(1000, resource.BinarySI)
	testCases := []struct {
		name  string
		usage uint64
		want  int64
	}{
		{name: "no usage", usage: 0, want: 1000},
		{name: "partial usage", usage: 400, want: 600},
		{name: "at limit", usage: 1000, want: 0},
		{name: "over limit clamps to zero", usage: 1500, want: 0},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionMemoryAvailable(limit, tc.usage)
			if got.Value() != tc.want {
				t.Errorf("partitionMemoryAvailable(%d) = %d, want %d", tc.usage, got.Value(), tc.want)
			}
		})
	}
}

func TestPartitionMemoryThresholdMet(t *testing.T) {
	limit := resource.NewQuantity(1000, resource.BinarySI)
	absoluteThreshold := evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)}
	percentageThreshold := evictionapi.ThresholdValue{Percentage: 0.1} // 10% of 1000 = 100

	testCases := []struct {
		name      string
		threshold evictionapi.ThresholdValue
		usage     uint64
		want      bool
	}{
		// Absolute threshold of 100 bytes available.
		{name: "absolute below threshold", threshold: absoluteThreshold, usage: 850, want: false}, // available 150 >= 100
		{name: "absolute at threshold", threshold: absoluteThreshold, usage: 900, want: false},    // available 100, not < 100
		{name: "absolute above threshold", threshold: absoluteThreshold, usage: 950, want: true},  // available 50 < 100
		{name: "absolute at limit", threshold: absoluteThreshold, usage: 1000, want: true},        // available 0 < 100
		// Percentage threshold: 10% of limit = 100 bytes available.
		{name: "percentage below threshold", threshold: percentageThreshold, usage: 850, want: false},
		{name: "percentage at threshold", threshold: percentageThreshold, usage: 900, want: false},
		{name: "percentage above threshold", threshold: percentageThreshold, usage: 950, want: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := partitionMemoryThresholdMet(tc.threshold, limit, tc.usage)
			if got != tc.want {
				t.Errorf("partitionMemoryThresholdMet(usage=%d) = %v, want %v", tc.usage, got, tc.want)
			}
		})
	}
}

func TestMemoryAvailableHardThreshold(t *testing.T) {
	hard := evictionapi.Threshold{
		Signal:   evictionapi.SignalMemoryAvailable,
		Operator: evictionapi.OpLessThan,
		Value:    evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)},
	}
	soft := evictionapi.Threshold{
		Signal:      evictionapi.SignalMemoryAvailable,
		Operator:    evictionapi.OpLessThan,
		Value:       evictionapi.ThresholdValue{Quantity: resource.NewQuantity(200, resource.BinarySI)},
		GracePeriod: time.Minute,
	}
	diskHard := evictionapi.Threshold{
		Signal:   evictionapi.SignalNodeFsAvailable,
		Operator: evictionapi.OpLessThan,
		Value:    evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)},
	}

	testCases := []struct {
		name       string
		thresholds []evictionapi.Threshold
		wantFound  bool
		wantValue  int64
	}{
		{name: "no thresholds", thresholds: nil, wantFound: false},
		{name: "only soft memory threshold", thresholds: []evictionapi.Threshold{soft}, wantFound: false},
		{name: "only disk hard threshold", thresholds: []evictionapi.Threshold{diskHard}, wantFound: false},
		{name: "hard memory threshold present", thresholds: []evictionapi.Threshold{soft, diskHard, hard}, wantFound: true, wantValue: 100},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := memoryAvailableHardThreshold(tc.thresholds)
			if found != tc.wantFound {
				t.Fatalf("memoryAvailableHardThreshold() found = %v, want %v", found, tc.wantFound)
			}
			if found && got.Value.Quantity.Value() != tc.wantValue {
				t.Errorf("memoryAvailableHardThreshold() value = %d, want %d", got.Value.Quantity.Value(), tc.wantValue)
			}
		})
	}
}

func TestFilterPodsByNamespaces(t *testing.T) {
	makeNsPod := func(name, ns string) *v1.Pod {
		pod := newPod(name, 0, nil, nil)
		pod.Namespace = ns
		return pod
	}
	kubeSystem1 := makeNsPod("ks1", "kube-system")
	defaultPod := makeNsPod("d1", "default")
	kubeSystem2 := makeNsPod("ks2", "kube-system")
	userPod := makeNsPod("u1", "user")
	pods := []*v1.Pod{kubeSystem1, defaultPod, kubeSystem2, userPod}

	testCases := []struct {
		name       string
		namespaces sets.Set[string]
		want       []*v1.Pod
	}{
		{name: "empty set yields nothing", namespaces: sets.New[string](), want: nil},
		{name: "single namespace preserves order", namespaces: sets.New("kube-system"), want: []*v1.Pod{kubeSystem1, kubeSystem2}},
		{name: "multiple namespaces", namespaces: sets.New("kube-system", "default"), want: []*v1.Pod{kubeSystem1, defaultPod, kubeSystem2}},
		{name: "no match", namespaces: sets.New("nonexistent"), want: nil},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterPodsByNamespaces(pods, tc.namespaces)
			if len(got) != len(tc.want) {
				t.Fatalf("filterPodsByNamespaces() returned %d pods, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("filterPodsByNamespaces()[%d] = %s, want %s", i, got[i].Name, tc.want[i].Name)
				}
			}
		})
	}
}

func TestPartitionMemoryEviction(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	const systemNs = "kube-system"

	hardThreshold := evictionapi.Threshold{
		Signal:   evictionapi.SignalMemoryAvailable,
		Operator: evictionapi.OpLessThan,
		Value:    evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)},
	}
	partition := &SystemPartitionConfig{
		MemoryLimit: resource.NewQuantity(1<<30, resource.BinarySI),
		CgroupPath:  "/kubepods/system",
		Namespaces:  sets.New(systemNs),
	}

	// system-partition pods: a guaranteed pod using less than its request and a
	// besteffort pod exceeding its (zero) request. A user besteffort pod with the
	// highest usage of all is included to ensure it is never selected.
	sysGuaranteed, sysGuaranteedStats := makePodWithMemoryStats("system-guaranteed", highPriority, newResourceList("100m", "1Gi", ""), newResourceList("100m", "1Gi", ""), "500Mi")
	sysGuaranteed.Namespace = systemNs
	sysBestEffort, sysBestEffortStats := makePodWithMemoryStats("system-besteffort", highPriority, newResourceList("", "", ""), newResourceList("", "", ""), "400Mi")
	sysBestEffort.Namespace = systemNs
	userBestEffort, userBestEffortStats := makePodWithMemoryStats("user-besteffort", lowPriority, newResourceList("", "", ""), newResourceList("", "", ""), "900Mi")
	userBestEffort.Namespace = "default"

	podStats := map[*v1.Pod]statsapi.PodStats{
		sysGuaranteed:  sysGuaranteedStats,
		sysBestEffort:  sysBestEffortStats,
		userBestEffort: userBestEffortStats,
	}
	_, statsFunc := makeSignalObservations(logger, makeMemoryStats("2Gi", podStats))

	newManager := func(podKiller *mockPodKiller, pressure bool, systemPartition *SystemPartitionConfig) *managerImpl {
		return &managerImpl{
			killPodFunc:             podKiller.killPodNow,
			recorder:                &record.FakeRecorder{},
			config:                  Config{Thresholds: []evictionapi.Threshold{hardThreshold}},
			signalToRankFunc:        buildSignalToRankFunc(false, false),
			systemPartition:         systemPartition,
			partitionMemoryPressure: pressure,
		}
	}

	t.Run("evicts highest-ranked system pod and ignores user pods", func(t *testing.T) {
		podKiller := &mockPodKiller{}
		m := newManager(podKiller, true, partition)
		// user pod has the highest usage overall but is not in the system partition.
		activePods := []*v1.Pod{sysGuaranteed, userBestEffort, sysBestEffort}
		evicted := m.partitionMemoryEviction(logger, activePods, statsFunc)
		if len(evicted) != 1 {
			t.Fatalf("expected exactly one evicted pod, got %d", len(evicted))
		}
		// besteffort ranks ahead of guaranteed within the partition.
		if evicted[0] != sysBestEffort {
			t.Errorf("evicted pod = %s, want %s", evicted[0].Name, sysBestEffort.Name)
		}
		if podKiller.pod != sysBestEffort {
			t.Errorf("killed pod = %v, want %s", podKiller.pod, sysBestEffort.Name)
		}
	})

	t.Run("no eviction when partition is not under pressure", func(t *testing.T) {
		podKiller := &mockPodKiller{}
		m := newManager(podKiller, false, partition)
		activePods := []*v1.Pod{sysGuaranteed, sysBestEffort}
		if evicted := m.partitionMemoryEviction(logger, activePods, statsFunc); evicted != nil {
			t.Errorf("expected no eviction, got %v", evicted)
		}
		if podKiller.pod != nil {
			t.Errorf("expected no pod killed, killed %s", podKiller.pod.Name)
		}
	})

	t.Run("no eviction when system partition not configured", func(t *testing.T) {
		podKiller := &mockPodKiller{}
		m := newManager(podKiller, true, nil)
		activePods := []*v1.Pod{sysGuaranteed, sysBestEffort}
		if evicted := m.partitionMemoryEviction(logger, activePods, statsFunc); evicted != nil {
			t.Errorf("expected no eviction, got %v", evicted)
		}
		if podKiller.pod != nil {
			t.Errorf("expected no pod killed, killed %s", podKiller.pod.Name)
		}
	})

	t.Run("no eviction when no system-partition pods are active", func(t *testing.T) {
		podKiller := &mockPodKiller{}
		m := newManager(podKiller, true, partition)
		activePods := []*v1.Pod{userBestEffort}
		if evicted := m.partitionMemoryEviction(logger, activePods, statsFunc); evicted != nil {
			t.Errorf("expected no eviction, got %v", evicted)
		}
		if podKiller.pod != nil {
			t.Errorf("expected no pod killed, killed %s", podKiller.pod.Name)
		}
	})
}

// TestPartitionMemoryEvictionNodeWideIncludesSystemPods verifies that node-wide
// memory eviction is unchanged by the system partition: when the node-level
// memory threshold fires, all pods (including system-partition pods) are eviction
// candidates and ranked together.
func TestPartitionMemoryEvictionNodeWideIncludesSystemPods(t *testing.T) {
	_, tCtx := ktesting.NewTestContext(t)

	// A system-partition besteffort pod ranks ahead of a user guaranteed pod under
	// memory pressure and must be selected by node-wide eviction.
	sysBestEffort, sysStats := makePodWithMemoryStats("system-besteffort", lowPriority, newResourceList("", "", ""), newResourceList("", "", ""), "800Mi")
	sysBestEffort.Namespace = "kube-system"
	userGuaranteed, userStats := makePodWithMemoryStats("user-guaranteed", highPriority, newResourceList("100m", "1Gi", ""), newResourceList("100m", "1Gi", ""), "500Mi")
	userGuaranteed.Namespace = "default"

	pods := []*v1.Pod{userGuaranteed, sysBestEffort}
	podStats := map[*v1.Pod]statsapi.PodStats{
		userGuaranteed: userStats,
		sysBestEffort:  sysStats,
	}
	activePodsFunc := func() []*v1.Pod { return pods }

	podKiller := &mockPodKiller{}
	diskInfoProvider := &mockDiskInfoProvider{dedicatedImageFs: ptr.To(false)}
	diskGC := &mockDiskGC{err: nil}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test")}

	config := Config{
		MaxPodGracePeriodSeconds: 5,
		PressureTransitionPeriod: time.Minute * 5,
		Thresholds: []evictionapi.Threshold{
			{
				Signal:   evictionapi.SignalMemoryAvailable,
				Operator: evictionapi.OpLessThan,
				Value:    evictionapi.ThresholdValue{Quantity: quantityMustParse("1Gi")},
			},
		},
		SystemPartition: &SystemPartitionConfig{
			MemoryLimit: resource.NewQuantity(4<<30, resource.BinarySI),
			CgroupPath:  "/kubepods/system",
			Namespaces:  sets.New("kube-system"),
		},
	}
	// node available (500Mi) is below the 1Gi hard threshold, triggering node-wide
	// eviction. The partition has ample headroom, so partition eviction does not fire.
	summaryProvider := &fakeSummaryProvider{result: makeMemoryStats("500Mi", podStats)}
	manager := &managerImpl{
		clock:                        testingclock.NewFakeClock(time.Now()),
		killPodFunc:                  podKiller.killPodNow,
		imageGC:                      diskGC,
		containerGC:                  diskGC,
		config:                       config,
		recorder:                     &record.FakeRecorder{},
		summaryProvider:              summaryProvider,
		nodeRef:                      nodeRef,
		nodeConditionsLastObservedAt: nodeConditionsObservedAt{},
		thresholdsFirstObservedAt:    thresholdsObservedAt{},
		systemPartition:              config.SystemPartition,
		partitionMemoryReader:        func(string) (uint64, error) { return 0, nil },
	}

	if _, err := manager.synchronize(tCtx, diskInfoProvider, activePodsFunc); err != nil {
		t.Fatalf("Manager expects no error but got %v", err)
	}
	if !manager.IsUnderMemoryPressure() {
		t.Errorf("Manager should report node memory pressure")
	}
	if manager.IsUnderPartitionMemoryPressure() {
		t.Errorf("Manager should not report partition memory pressure")
	}
	if podKiller.pod == nil {
		t.Fatalf("expected node-wide eviction to kill a pod, but none was killed")
	}
	if podKiller.pod != sysBestEffort {
		t.Errorf("node-wide eviction killed %s, want %s (system-partition pods must remain candidates)", podKiller.pod.Name, sysBestEffort.Name)
	}
}

func TestSynchronizePartitionMemory(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	hardThreshold := evictionapi.Threshold{
		Signal:   evictionapi.SignalMemoryAvailable,
		Operator: evictionapi.OpLessThan,
		Value:    evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)},
	}

	testCases := []struct {
		name            string
		systemPartition *SystemPartitionConfig
		usage           uint64
		readErr         error
		wantPressure    bool
	}{
		{
			name:            "no system partition configured",
			systemPartition: nil,
			wantPressure:    false,
		},
		{
			name: "usage below threshold - no pressure",
			systemPartition: &SystemPartitionConfig{
				MemoryLimit: resource.NewQuantity(1000, resource.BinarySI),
				CgroupPath:  "/kubepods/system",
			},
			usage:        500, // available 500 >= 100
			wantPressure: false,
		},
		{
			name: "usage above threshold - pressure",
			systemPartition: &SystemPartitionConfig{
				MemoryLimit: resource.NewQuantity(1000, resource.BinarySI),
				CgroupPath:  "/kubepods/system",
			},
			usage:        950, // available 50 < 100
			wantPressure: true,
		},
		{
			name: "read error - pressure not updated",
			systemPartition: &SystemPartitionConfig{
				MemoryLimit: resource.NewQuantity(1000, resource.BinarySI),
				CgroupPath:  "/kubepods/system",
			},
			readErr:      fmt.Errorf("cgroup not found"),
			wantPressure: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := &managerImpl{
				config:          Config{Thresholds: []evictionapi.Threshold{hardThreshold}},
				systemPartition: tc.systemPartition,
				partitionMemoryReader: func(string) (uint64, error) {
					return tc.usage, tc.readErr
				},
			}
			m.synchronizePartitionMemory(logger)
			if got := m.IsUnderPartitionMemoryPressure(); got != tc.wantPressure {
				t.Errorf("IsUnderPartitionMemoryPressure() = %v, want %v", got, tc.wantPressure)
			}
		})
	}
}
