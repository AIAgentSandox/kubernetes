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

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	"k8s.io/kubernetes/test/utils/ktesting"
	testingclock "k8s.io/utils/clock/testing"
)

// fakePartitionStatsProvider is a test PartitionStatsProvider that returns
// canned stats or an error.
type fakePartitionStatsProvider struct {
	stats *PartitionStats
	err   error
}

func (f *fakePartitionStatsProvider) GetPartitionStats() (*PartitionStats, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.stats, nil
}

func TestNewPartitionManager(t *testing.T) {
	podKiller := &mockPodKiller{}
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test", UID: types.UID("test")}
	recorder := &record.FakeRecorder{}
	fakeClock := testingclock.NewFakeClock(time.Now())
	thresholds := []evictionapi.Threshold{
		{
			Signal:   evictionapi.SignalMemoryAvailable,
			Operator: evictionapi.OpLessThan,
			Value:    evictionapi.ThresholdValue{Quantity: resource.NewQuantity(100, resource.BinarySI)},
		},
	}
	statsProvider := &fakePartitionStatsProvider{stats: &PartitionStats{MemoryUsageBytes: 10, MemoryLimitBytes: 100}}

	mgr := NewPartitionManager("system", statsProvider, thresholds, podKiller.killPodNow, recorder, nodeRef, fakeClock)

	impl, ok := mgr.(*managerImpl)
	if !ok {
		t.Fatalf("NewPartitionManager returned %T, want *managerImpl", mgr)
	}
	if impl.partitionName != "system" {
		t.Errorf("partitionName = %q, want %q", impl.partitionName, "system")
	}
	if len(impl.config.Thresholds) != 1 || impl.config.Thresholds[0].Signal != evictionapi.SignalMemoryAvailable {
		t.Errorf("config.Thresholds = %+v, want the passed memory.available threshold", impl.config.Thresholds)
	}
	// Partition managers do not reclaim node-level resources: GC must be no-ops.
	if _, ok := impl.imageGC.(noopImageGC); !ok {
		t.Errorf("imageGC = %T, want noopImageGC", impl.imageGC)
	}
	if _, ok := impl.containerGC.(noopContainerGC); !ok {
		t.Errorf("containerGC = %T, want noopContainerGC", impl.containerGC)
	}
	// The summary provider must be the partition adapter, not a node summary provider.
	if _, ok := impl.summaryProvider.(*partitionSummaryProvider); !ok {
		t.Errorf("summaryProvider = %T, want *partitionSummaryProvider", impl.summaryProvider)
	}
	// Partition managers do not carry the node-wide partition config.
	if impl.systemPartition != nil {
		t.Errorf("systemPartition = %+v, want nil", impl.systemPartition)
	}
	if impl.nodeRef != nodeRef {
		t.Errorf("nodeRef = %+v, want %+v", impl.nodeRef, nodeRef)
	}
	if impl.clock != fakeClock {
		t.Errorf("clock not wired to the provided clock")
	}
}

func TestNoopGC(t *testing.T) {
	_, tCtx := ktesting.NewTestContext(t)
	if err := (noopImageGC{}).DeleteUnusedImages(tCtx); err != nil {
		t.Errorf("noopImageGC.DeleteUnusedImages returned error: %v", err)
	}
	if err := (noopContainerGC{}).DeleteAllUnusedContainers(tCtx); err != nil {
		t.Errorf("noopContainerGC.DeleteAllUnusedContainers returned error: %v", err)
	}
}

func TestPartitionSummaryProviderGet(t *testing.T) {
	testCases := map[string]struct {
		stats          *PartitionStats
		err            error
		wantErr        bool
		wantAvailable  uint64
		wantWorkingSet uint64
	}{
		"usage below limit": {
			stats:          &PartitionStats{MemoryUsageBytes: 400, MemoryLimitBytes: 1024},
			wantAvailable:  624,
			wantWorkingSet: 400,
		},
		"usage equal to limit": {
			stats:          &PartitionStats{MemoryUsageBytes: 1024, MemoryLimitBytes: 1024},
			wantAvailable:  0,
			wantWorkingSet: 1024,
		},
		"usage above limit clamps available to zero": {
			stats:          &PartitionStats{MemoryUsageBytes: 2048, MemoryLimitBytes: 1024},
			wantAvailable:  0,
			wantWorkingSet: 2048,
		},
		"read error is propagated": {
			err:     errors.New("boom"),
			wantErr: true,
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			_, tCtx := ktesting.NewTestContext(t)
			now := time.Now()
			fakeClock := testingclock.NewFakeClock(now)
			provider := newPartitionSummaryProvider(&fakePartitionStatsProvider{stats: tc.stats, err: tc.err}, fakeClock)

			summary, err := provider.Get(tCtx, true)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if summary != nil {
					t.Fatalf("expected nil summary on error, got %+v", summary)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if summary.Node.Memory == nil {
				t.Fatalf("summary.Node.Memory is nil")
			}
			if summary.Node.Memory.AvailableBytes == nil || *summary.Node.Memory.AvailableBytes != tc.wantAvailable {
				t.Errorf("AvailableBytes = %v, want %d", summary.Node.Memory.AvailableBytes, tc.wantAvailable)
			}
			if summary.Node.Memory.WorkingSetBytes == nil || *summary.Node.Memory.WorkingSetBytes != tc.wantWorkingSet {
				t.Errorf("WorkingSetBytes = %v, want %d", summary.Node.Memory.WorkingSetBytes, tc.wantWorkingSet)
			}
			if !summary.Node.Memory.Time.Time.Equal(now) {
				t.Errorf("Time = %v, want %v", summary.Node.Memory.Time.Time, now)
			}
		})
	}
}

// TestPartitionSummaryProviderObservations verifies that the summary produced by
// the adapter feeds makeSignalObservations into a memory.available signal whose
// available value is (limit - usage) and whose capacity is the partition limit.
// This is the property the node-level eviction logic depends on to evaluate
// partition thresholds correctly.
func TestPartitionSummaryProviderObservations(t *testing.T) {
	logger, tCtx := ktesting.NewTestContext(t)
	const usage = uint64(600)
	const limit = uint64(1024)
	fakeClock := testingclock.NewFakeClock(time.Now())
	provider := newPartitionSummaryProvider(&fakePartitionStatsProvider{stats: &PartitionStats{MemoryUsageBytes: usage, MemoryLimitBytes: limit}}, fakeClock)

	summary, err := provider.Get(tCtx, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	observations, _ := makeSignalObservations(logger, summary)
	obs, found := observations[evictionapi.SignalMemoryAvailable]
	if !found {
		t.Fatalf("no memory.available observation produced")
	}
	if got := obs.available.Value(); got != int64(limit-usage) {
		t.Errorf("available = %d, want %d", got, limit-usage)
	}
	if got := obs.capacity.Value(); got != int64(limit) {
		t.Errorf("capacity = %d, want %d (the partition limit)", got, limit)
	}
}
