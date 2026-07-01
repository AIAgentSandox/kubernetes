//go:build linux

/*
Copyright 2016 The Kubernetes Authors.

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
	"os"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2/ktesting"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestIsCgroupPod(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	qosContainersInfo := QOSContainersInfo{
		Guaranteed: RootCgroupName,
		Burstable:  NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBestEffort))),
	}
	podUID := types.UID("123")
	testCases := []struct {
		input          CgroupName
		expectedResult bool
		expectedUID    types.UID
	}{
		{
			input:          RootCgroupName,
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.Guaranteed),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.Guaranteed, GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			input:          NewCgroupName(qosContainersInfo.Guaranteed, GetPodCgroupNameSuffix(podUID), "container.scope"),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.Burstable),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.Burstable, GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			input:          NewCgroupName(qosContainersInfo.Burstable, GetPodCgroupNameSuffix(podUID), "container.scope"),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.BestEffort),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(qosContainersInfo.BestEffort, GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			input:          NewCgroupName(qosContainersInfo.BestEffort, GetPodCgroupNameSuffix(podUID), "container.scope"),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(RootCgroupName, "system"),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			input:          NewCgroupName(RootCgroupName, "system", "kubelet"),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
		{
			// system partition guaranteed pod (kubepods/system/pod<uid>)
			input:          NewCgroupName(RootCgroupName, "system", GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			// system partition burstable pod (kubepods/system/burstable/pod<uid>)
			input:          NewCgroupName(NewCgroupName(RootCgroupName, "system"), strings.ToLower(string(v1.PodQOSBurstable)), GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			// system partition besteffort pod (kubepods/system/besteffort/pod<uid>)
			input:          NewCgroupName(NewCgroupName(RootCgroupName, "system"), strings.ToLower(string(v1.PodQOSBestEffort)), GetPodCgroupNameSuffix(podUID)),
			expectedResult: true,
			expectedUID:    podUID,
		},
		{
			// contains reserved word "pod" in cgroup name
			input:          NewCgroupName(RootCgroupName, GetPodCgroupNameSuffix("this-uid-contains-reserved-word-pod")),
			expectedResult: false,
			expectedUID:    types.UID(""),
		},
	}
	for _, cgroupDriver := range []string{"cgroupfs", "systemd"} {
		pcm := &podContainerManagerImpl{
			cgroupManager:     NewCgroupManager(logger, nil, cgroupDriver),
			enforceCPULimits:  true,
			qosContainersInfo: qosContainersInfo,
		}
		for _, testCase := range testCases {
			// Give the right cgroup structure based on whether systemd is enabled.
			var name string
			if cgroupDriver == "systemd" {
				name = testCase.input.ToSystemd()
			} else {
				name = testCase.input.ToCgroupfs()
			}
			// check if this is a pod or not with the literal cgroupfs input
			result, resultUID := pcm.IsPodCgroup(name)
			if result != testCase.expectedResult {
				t.Errorf("Unexpected result for driver: %v, input: %v, expected: %v, actual: %v", cgroupDriver, testCase.input, testCase.expectedResult, result)
			}
			if resultUID != testCase.expectedUID {
				t.Errorf("Unexpected result for driver: %v, input: %v, expected: %v, actual: %v", cgroupDriver, testCase.input, testCase.expectedUID, resultUID)
			}

		}
	}
}

func TestGetPodContainerName(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	newGuaranteedPodWithUID := func(uid types.UID) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID: uid,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000m"),
								v1.ResourceMemory: resource.MustParse("1G"),
							},
							Limits: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000m"),
								v1.ResourceMemory: resource.MustParse("1G"),
							},
						},
					},
				},
			},
		}
	}
	newBurstablePodWithUID := func(uid types.UID) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID: uid,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container",
						Resources: v1.ResourceRequirements{
							Requests: v1.ResourceList{
								v1.ResourceCPU:    resource.MustParse("1000m"),
								v1.ResourceMemory: resource.MustParse("1G"),
							},
						},
					},
				},
			},
		}
	}
	newBestEffortPodWithUID := func(uid types.UID) *v1.Pod {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID: uid,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name: "container",
					},
				},
			},
		}
	}

	qosContainersInfo := QOSContainersInfo{
		Guaranteed: RootCgroupName,
		Burstable:  NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBestEffort))),
	}

	type fields struct {
		cgroupManager CgroupManager
	}
	type args struct {
		pod *v1.Pod
	}

	tests := []struct {
		name                string
		fields              fields
		args                args
		wantCgroupName      CgroupName
		wantLiteralCgroupfs string
	}{
		{
			name: "pod with qos guaranteed and cgroupfs",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "cgroupfs"),
			},
			args: args{
				pod: newGuaranteedPodWithUID("fake-uid-1"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.Guaranteed, "podfake-uid-1"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.Guaranteed, "podfake-uid-1").ToCgroupfs(),
		}, {
			name: "pod with qos guaranteed and systemd",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "systemd"),
			},
			args: args{
				pod: newGuaranteedPodWithUID("fake-uid-2"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.Guaranteed, "podfake-uid-2"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.Guaranteed, "podfake-uid-2").ToSystemd(),
		}, {
			name: "pod with qos burstable and cgroupfs",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "cgroupfs"),
			},
			args: args{
				pod: newBurstablePodWithUID("fake-uid-3"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.Burstable, "podfake-uid-3"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.Burstable, "podfake-uid-3").ToCgroupfs(),
		}, {
			name: "pod with qos burstable and systemd",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "systemd"),
			},
			args: args{
				pod: newBurstablePodWithUID("fake-uid-4"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.Burstable, "podfake-uid-4"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.Burstable, "podfake-uid-4").ToSystemd(),
		}, {
			name: "pod with qos best-effort and cgroupfs",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "cgroupfs"),
			},
			args: args{
				pod: newBestEffortPodWithUID("fake-uid-5"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.BestEffort, "podfake-uid-5"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.BestEffort, "podfake-uid-5").ToCgroupfs(),
		}, {
			name: "pod with qos best-effort and systemd",
			fields: fields{
				cgroupManager: NewCgroupManager(logger, nil, "systemd"),
			},
			args: args{
				pod: newBestEffortPodWithUID("fake-uid-6"),
			},
			wantCgroupName:      NewCgroupName(qosContainersInfo.BestEffort, "podfake-uid-6"),
			wantLiteralCgroupfs: NewCgroupName(qosContainersInfo.BestEffort, "podfake-uid-6").ToSystemd(),
		},
	}

	for _, tt := range tests {
		pcm := &podContainerManagerImpl{
			cgroupManager:     tt.fields.cgroupManager,
			qosContainersInfo: qosContainersInfo,
		}

		t.Run(tt.name, func(t *testing.T) {
			actualCgroupName, actualLiteralCgroupfs := pcm.GetPodContainerName(tt.args.pod)
			require.Equalf(t, tt.wantCgroupName, actualCgroupName, "Unexpected cgroup name for pod with UID %s, container resources: %v", tt.args.pod.UID, tt.args.pod.Spec.Containers[0].Resources)
			require.Equalf(t, tt.wantLiteralCgroupfs, actualLiteralCgroupfs, "Unexpected literal cgroupfs for pod with UID %s, container resources: %v", tt.args.pod.UID, tt.args.pod.Spec.Containers[0].Resources)
		})
	}
}

// TestGetAllPodsFromCgroupsSystemPartition verifies that orphaned pod cgroup
// discovery scans both the default partition (kubepods) and the system
// partition (kubepods/system) hierarchies. The system partition is scanned
// whether or not systemQOSContainersInfo is populated so that leftover pod
// cgroups are still detected after the feature is disabled.
func TestGetAllPodsFromCgroupsSystemPartition(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)
	tmpDir := t.TempDir()

	cgroupManager := NewCgroupManager(logger, nil, "cgroupfs")

	kubepods := NewCgroupName(RootCgroupName, "kubepods")
	defaultQOS := QOSContainersInfo{
		Guaranteed: kubepods,
		Burstable:  NewCgroupName(kubepods, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(kubepods, strings.ToLower(string(v1.PodQOSBestEffort))),
	}
	systemRoot := NewCgroupName(kubepods, "system")
	systemQOS := QOSContainersInfo{
		Guaranteed: systemRoot,
		Burstable:  NewCgroupName(systemRoot, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(systemRoot, strings.ToLower(string(v1.PodQOSBestEffort))),
	}

	// pod UID -> parent QoS cgroup where the pod cgroup lives on disk.
	podPlacements := map[types.UID]CgroupName{
		"default-guar":  defaultQOS.Guaranteed,
		"default-burst": defaultQOS.Burstable,
		"default-be":    defaultQOS.BestEffort,
		"system-guar":   systemQOS.Guaranteed,
		"system-burst":  systemQOS.Burstable,
		"system-be":     systemQOS.BestEffort,
	}
	for uid, parent := range podPlacements {
		podCgroup := NewCgroupName(parent, GetPodCgroupNameSuffix(uid))
		dir := path.Join(tmpDir, cgroupManager.Name(podCgroup))
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	testCases := []struct {
		name                    string
		systemQOSContainersInfo QOSContainersInfo
	}{
		{
			name:                    "feature enabled (system QoS info populated)",
			systemQOSContainersInfo: systemQOS,
		},
		{
			name:                    "feature disabled (system QoS info derived from default root)",
			systemQOSContainersInfo: QOSContainersInfo{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := &podContainerManagerImpl{
				cgroupManager:           cgroupManager,
				qosContainersInfo:       defaultQOS,
				systemQOSContainersInfo: tc.systemQOSContainersInfo,
				subsystems: &CgroupSubsystems{
					MountPoints: map[string]string{"memory": tmpDir},
				},
			}

			foundPods, err := pcm.GetAllPodsFromCgroups()
			require.NoError(t, err)
			require.Len(t, foundPods, len(podPlacements))

			for uid, parent := range podPlacements {
				want := NewCgroupName(parent, GetPodCgroupNameSuffix(uid))
				got, ok := foundPods[uid]
				require.Truef(t, ok, "pod %q not discovered", uid)
				assert.Equalf(t, want.ToCgroupfs(), got.ToCgroupfs(), "unexpected cgroup path for pod %q", uid)
			}
		})
	}
}

// TestGetPodContainerNameSystemPartition verifies that GetPodContainerName routes
// pods to the system partition QoS hierarchy when the pod's namespace is in the
// configured system namespaces, and to the default hierarchy otherwise.
func TestGetPodContainerNameSystemPartition(t *testing.T) {
	logger, _ := ktesting.NewTestContext(t)

	newPod := func(uid types.UID, namespace string, qos v1.PodQOSClass) *v1.Pod {
		var resources v1.ResourceRequirements
		switch qos {
		case v1.PodQOSGuaranteed:
			resources = v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000m"),
					v1.ResourceMemory: resource.MustParse("1G"),
				},
				Limits: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000m"),
					v1.ResourceMemory: resource.MustParse("1G"),
				},
			}
		case v1.PodQOSBurstable:
			resources = v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:    resource.MustParse("1000m"),
					v1.ResourceMemory: resource.MustParse("1G"),
				},
			}
		case v1.PodQOSBestEffort:
			// no requests or limits
		}
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				UID:       uid,
				Namespace: namespace,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:      "container",
						Resources: resources,
					},
				},
			},
		}
	}

	// default partition QoS containers (kubepods/...)
	defaultQOS := QOSContainersInfo{
		Guaranteed: RootCgroupName,
		Burstable:  NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(RootCgroupName, strings.ToLower(string(v1.PodQOSBestEffort))),
	}
	// system partition QoS containers (kubepods/system/...)
	systemRoot := NewCgroupName(RootCgroupName, "system")
	systemQOS := QOSContainersInfo{
		Guaranteed: systemRoot,
		Burstable:  NewCgroupName(systemRoot, strings.ToLower(string(v1.PodQOSBurstable))),
		BestEffort: NewCgroupName(systemRoot, strings.ToLower(string(v1.PodQOSBestEffort))),
	}

	tests := []struct {
		name             string
		systemNamespaces sets.Set[string]
		pod              *v1.Pod
		wantParent       CgroupName
	}{
		{
			name:             "system namespace + guaranteed -> system guaranteed",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-1", "kube-system", v1.PodQOSGuaranteed),
			wantParent:       systemQOS.Guaranteed,
		},
		{
			name:             "system namespace + burstable -> system burstable",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-2", "kube-system", v1.PodQOSBurstable),
			wantParent:       systemQOS.Burstable,
		},
		{
			name:             "system namespace + besteffort -> system besteffort",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-3", "kube-system", v1.PodQOSBestEffort),
			wantParent:       systemQOS.BestEffort,
		},
		{
			name:             "non-system namespace + guaranteed -> default guaranteed",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-4", "default", v1.PodQOSGuaranteed),
			wantParent:       defaultQOS.Guaranteed,
		},
		{
			name:             "non-system namespace + burstable -> default burstable",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-5", "default", v1.PodQOSBurstable),
			wantParent:       defaultQOS.Burstable,
		},
		{
			name:             "non-system namespace + besteffort -> default besteffort",
			systemNamespaces: sets.New("kube-system"),
			pod:              newPod("uid-6", "default", v1.PodQOSBestEffort),
			wantParent:       defaultQOS.BestEffort,
		},
		{
			name:             "feature off (empty system namespaces) + kube-system pod -> default",
			systemNamespaces: nil,
			pod:              newPod("uid-7", "kube-system", v1.PodQOSGuaranteed),
			wantParent:       defaultQOS.Guaranteed,
		},
		{
			name:             "feature off (empty system namespaces) + burstable -> default",
			systemNamespaces: nil,
			pod:              newPod("uid-8", "kube-system", v1.PodQOSBurstable),
			wantParent:       defaultQOS.Burstable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pcm := &podContainerManagerImpl{
				cgroupManager:           NewCgroupManager(logger, nil, "cgroupfs"),
				qosContainersInfo:       defaultQOS,
				systemQOSContainersInfo: systemQOS,
				systemNamespaces:        tt.systemNamespaces,
			}
			wantCgroupName := NewCgroupName(tt.wantParent, GetPodCgroupNameSuffix(tt.pod.UID))
			actualCgroupName, actualLiteralCgroupfs := pcm.GetPodContainerName(tt.pod)
			require.Equal(t, wantCgroupName, actualCgroupName, "Unexpected cgroup name")
			require.Equal(t, wantCgroupName.ToCgroupfs(), actualLiteralCgroupfs, "Unexpected literal cgroupfs")
		})
	}
}
