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

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2/ktesting"
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
