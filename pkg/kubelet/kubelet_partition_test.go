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

package kubelet

import (
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
	testingclock "k8s.io/utils/clock/testing"
)

// noopKillPod is a KillPodFunc that performs no work; the partition eviction
// manager only invokes it during eviction, which these construction tests do not
// exercise.
func noopKillPod(_ *v1.Pod, _ bool, _ *int64, _ func(*v1.PodStatus)) error {
	return nil
}

func TestNewPartitionEvictionManagers(t *testing.T) {
	nodeRef := &v1.ObjectReference{Kind: "Node", Name: "test-node"}
	recorder := record.NewFakeRecorder(10)
	fakeClock := testingclock.NewFakeClock(time.Now())
	hardThreshold := evictionapi.Threshold{
		Signal:   evictionapi.SignalMemoryAvailable,
		Operator: evictionapi.OpLessThan,
		Value: evictionapi.ThresholdValue{
			Quantity: resource.NewQuantity(100*1024*1024, resource.BinarySI),
		},
	}
	configured := kubeletconfiginternal.SystemPartitionConfiguration{
		MemoryLimit: resource.MustParse("1Gi"),
		Namespaces:  []string{"kube-system"},
	}

	testCases := []struct {
		name             string
		featureEnabled   bool
		systemPartition  kubeletconfiginternal.SystemPartitionConfiguration
		cgroupPath       string
		expectManager    bool
		expectNamespaces []string
	}{
		{
			name:            "feature disabled",
			featureEnabled:  false,
			systemPartition: configured,
			cgroupPath:      "/kubepods/system",
			expectManager:   false,
		},
		{
			name:            "no partition namespaces configured",
			featureEnabled:  true,
			systemPartition: kubeletconfiginternal.SystemPartitionConfiguration{MemoryLimit: resource.MustParse("1Gi")},
			cgroupPath:      "/kubepods/system",
			expectManager:   false,
		},
		{
			name:            "partition cgroup does not exist",
			featureEnabled:  true,
			systemPartition: configured,
			cgroupPath:      "",
			expectManager:   false,
		},
		{
			name:             "system partition configured",
			featureEnabled:   true,
			systemPartition:  configured,
			cgroupPath:       "/kubepods/system",
			expectManager:    true,
			expectNamespaces: []string{"kube-system"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.NodeSystemPartition, tc.featureEnabled)

			managers, namespaces := newPartitionEvictionManagers(
				tc.systemPartition,
				tc.cgroupPath,
				[]evictionapi.Threshold{hardThreshold},
				noopKillPod,
				recorder,
				nodeRef,
				fakeClock,
			)

			if tc.expectManager {
				manager, ok := managers[eviction.SystemPartitionName]
				if !ok {
					t.Fatalf("expected a %q partition eviction manager, got none", eviction.SystemPartitionName)
				}
				if manager == nil {
					t.Fatalf("expected a non-nil %q partition eviction manager", eviction.SystemPartitionName)
				}
				// A freshly constructed manager has not observed any pressure yet.
				if manager.IsUnderMemoryPressure() {
					t.Errorf("expected new partition manager to not be under memory pressure")
				}
				ns, ok := namespaces[eviction.SystemPartitionName]
				if !ok {
					t.Fatalf("expected namespaces for partition %q", eviction.SystemPartitionName)
				}
				for _, want := range tc.expectNamespaces {
					if !ns.Has(want) {
						t.Errorf("expected partition namespaces to contain %q", want)
					}
				}
			} else {
				if len(managers) != 0 {
					t.Errorf("expected no partition managers, got %d", len(managers))
				}
				if len(namespaces) != 0 {
					t.Errorf("expected no partition namespaces, got %d", len(namespaces))
				}
			}
		})
	}
}
