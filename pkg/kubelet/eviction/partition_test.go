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
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/sets"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
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
