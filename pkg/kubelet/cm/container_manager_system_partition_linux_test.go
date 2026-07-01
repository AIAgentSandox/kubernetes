//go:build linux

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

package cm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"k8s.io/klog/v2/ktesting"
	kubefeatures "k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/utils/cpuset"
)

func TestSystemPartitionConfigured(t *testing.T) {
	tests := []struct {
		name           string
		featureEnabled bool
		config         kubeletconfig.SystemPartitionConfiguration
		expected       bool
	}{
		{
			name:           "feature disabled with namespaces configured",
			featureEnabled: false,
			config:         kubeletconfig.SystemPartitionConfiguration{Namespaces: []string{"kube-system"}},
			expected:       false,
		},
		{
			name:           "feature enabled with namespaces configured",
			featureEnabled: true,
			config:         kubeletconfig.SystemPartitionConfiguration{Namespaces: []string{"kube-system"}},
			expected:       true,
		},
		{
			name:           "feature enabled without namespaces",
			featureEnabled: true,
			config:         kubeletconfig.SystemPartitionConfiguration{},
			expected:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, kubefeatures.NodeSystemPartition, tc.featureEnabled)
			assert.Equal(t, tc.expected, systemPartitionConfigured(tc.config))
		})
	}
}

func TestCreateSystemPartitionCgroup(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	cgroupRoot := ParseCgroupfsToCgroupName("/")
	cgroupRoot = NewCgroupName(cgroupRoot, defaultNodeAllocatableCgroupName)
	name := NewCgroupName(cgroupRoot, systemPartitionCgroupBaseName)

	// The computed cgroup name must be nested directly under kubepods.
	assert.Equal(t, "/kubepods/system", name.ToCgroupfs())

	t.Run("creates cgroup with memory limit and cpuset", func(t *testing.T) {
		fake := &fakeCgroupManager{}
		config := kubeletconfig.SystemPartitionConfiguration{
			MemoryLimit: resource.MustParse("4Gi"),
			CPUSet:      "0-3",
			Namespaces:  []string{"kube-system"},
		}

		require.NoError(t, createSystemPartitionCgroup(logger, fake, name, config))
		require.Len(t, fake.created, 1)
		require.Empty(t, fake.updates)

		created := fake.created[0]
		assert.Equal(t, name.ToCgroupfs(), created.Name.ToCgroupfs())
		require.NotNil(t, created.ResourceParameters.Memory)
		assert.Equal(t, int64(4*1024*1024*1024), *created.ResourceParameters.Memory)
		assert.Equal(t, cpuset.New(0, 1, 2, 3), created.ResourceParameters.CPUSet)
	})

	t.Run("updates cgroup when it already exists", func(t *testing.T) {
		fake := &fakeCgroupManager{exists: true}
		config := kubeletconfig.SystemPartitionConfiguration{
			MemoryLimit: resource.MustParse("2Gi"),
			Namespaces:  []string{"kube-system"},
		}

		require.NoError(t, createSystemPartitionCgroup(logger, fake, name, config))
		require.Empty(t, fake.created)
		require.Len(t, fake.updates, 1)

		updated := fake.updates[0]
		require.NotNil(t, updated.ResourceParameters.Memory)
		assert.Equal(t, int64(2*1024*1024*1024), *updated.ResourceParameters.Memory)
	})

	t.Run("leaves memory and cpuset unset when not configured", func(t *testing.T) {
		fake := &fakeCgroupManager{}
		config := kubeletconfig.SystemPartitionConfiguration{
			Namespaces: []string{"kube-system"},
		}

		require.NoError(t, createSystemPartitionCgroup(logger, fake, name, config))
		require.Len(t, fake.created, 1)

		created := fake.created[0]
		assert.Nil(t, created.ResourceParameters.Memory)
		assert.True(t, created.ResourceParameters.CPUSet.IsEmpty())
	})

	t.Run("returns error for invalid cpuset", func(t *testing.T) {
		fake := &fakeCgroupManager{}
		config := kubeletconfig.SystemPartitionConfiguration{
			CPUSet:     "not-a-cpuset",
			Namespaces: []string{"kube-system"},
		}

		assert.Error(t, createSystemPartitionCgroup(logger, fake, name, config))
		assert.Empty(t, fake.created)
	})
}
