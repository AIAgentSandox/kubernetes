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
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
)

func TestCgroupPartitionStatsProviderGetPartitionStats(t *testing.T) {
	testCases := map[string]struct {
		cgroupPath   string
		memoryLimit  *resource.Quantity
		readUsage    uint64
		readErr      error
		wantErr      bool
		wantUsage    uint64
		wantLimit    uint64
		wantReadPath string
	}{
		"reports usage and limit": {
			cgroupPath:   "/kubepods/system",
			memoryLimit:  resource.NewQuantity(1024, resource.BinarySI),
			readUsage:    512,
			wantUsage:    512,
			wantLimit:    1024,
			wantReadPath: "/kubepods/system",
		},
		"nil limit reports zero limit": {
			cgroupPath:   "/kubepods/system",
			memoryLimit:  nil,
			readUsage:    256,
			wantUsage:    256,
			wantLimit:    0,
			wantReadPath: "/kubepods/system",
		},
		"non-positive limit reports zero limit": {
			cgroupPath:   "/kubepods/system",
			memoryLimit:  resource.NewQuantity(0, resource.BinarySI),
			readUsage:    128,
			wantUsage:    128,
			wantLimit:    0,
			wantReadPath: "/kubepods/system",
		},
		"read error is propagated": {
			cgroupPath:  "/kubepods/system",
			memoryLimit: resource.NewQuantity(1024, resource.BinarySI),
			readErr:     errors.New("boom"),
			wantErr:     true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			var gotPath string
			provider := &cgroupPartitionStatsProvider{
				cgroupPath:  tc.cgroupPath,
				memoryLimit: tc.memoryLimit,
				readMemoryUsage: func(path string) (uint64, error) {
					gotPath = path
					return tc.readUsage, tc.readErr
				},
			}

			stats, err := provider.GetPartitionStats()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if stats != nil {
					t.Fatalf("expected nil stats on error, got %+v", stats)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotPath != tc.wantReadPath {
				t.Errorf("read path = %q, want %q", gotPath, tc.wantReadPath)
			}
			if stats.MemoryUsageBytes != tc.wantUsage {
				t.Errorf("MemoryUsageBytes = %d, want %d", stats.MemoryUsageBytes, tc.wantUsage)
			}
			if stats.MemoryLimitBytes != tc.wantLimit {
				t.Errorf("MemoryLimitBytes = %d, want %d", stats.MemoryLimitBytes, tc.wantLimit)
			}
		})
	}
}

func TestNewCgroupPartitionStatsProvider(t *testing.T) {
	limit := resource.NewQuantity(2048, resource.BinarySI)
	provider := newCgroupPartitionStatsProvider("/kubepods/system", limit)
	if provider.cgroupPath != "/kubepods/system" {
		t.Errorf("cgroupPath = %q, want %q", provider.cgroupPath, "/kubepods/system")
	}
	if provider.memoryLimit != limit {
		t.Errorf("memoryLimit = %v, want %v", provider.memoryLimit, limit)
	}
	if provider.readMemoryUsage == nil {
		t.Errorf("readMemoryUsage should default to a non-nil reader")
	}
}

func TestPartitionThresholds(t *testing.T) {
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

	testCases := map[string]struct {
		thresholds []evictionapi.Threshold
		want       []evictionapi.Threshold
	}{
		"selects hard memory threshold": {
			thresholds: []evictionapi.Threshold{soft, diskHard, hard},
			want:       []evictionapi.Threshold{hard},
		},
		"ignores soft memory threshold": {
			thresholds: []evictionapi.Threshold{soft},
			want:       nil,
		},
		"ignores non-memory thresholds": {
			thresholds: []evictionapi.Threshold{diskHard},
			want:       nil,
		},
		"empty input": {
			thresholds: nil,
			want:       nil,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			got := PartitionThresholds(tc.thresholds)
			if len(got) != len(tc.want) {
				t.Fatalf("PartitionThresholds() returned %d thresholds, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Signal != tc.want[i].Signal || got[i].Value.Quantity.Cmp(*tc.want[i].Value.Quantity) != 0 {
					t.Errorf("PartitionThresholds()[%d] = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}
