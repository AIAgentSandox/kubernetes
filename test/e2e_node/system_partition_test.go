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

package e2enode

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	imageutils "k8s.io/kubernetes/test/utils/image"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

const (
	// systemPartitionMemoryLimit is the memory.max applied to kubepods/system.
	systemPartitionMemoryLimit = "1Gi"
	// systemPartitionCPUSet is the cpuset.cpus applied to kubepods/system.
	systemPartitionCPUSet = "0"
)

// systemPartitionCgroupName returns the internal CgroupName for the
// kubepods/system partition root.
func systemPartitionCgroupName() cm.CgroupName {
	kubepods := cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup)
	return cm.NewCgroupName(kubepods, "system")
}

// systemPartitionCgroupFsPath returns the host cgroup v2 path for a cgroup
// under the kubepods/system partition. The relative components are appended to
// the partition root (for example "burstable" or "pod<UID>").
func systemPartitionCgroupFsPath(components ...string) string {
	name := systemPartitionCgroupName()
	if len(components) > 0 {
		name = cm.NewCgroupName(name, components...)
	}
	return filepath.Join(cgroupRoot, toCgroupFsName(name))
}

// makePodToVerifyCgroupsAbsent returns a pod that succeeds only when none of
// the given cgroups (relative to the kubepods root) exist. It mirrors
// makePodToVerifyCgroups but inverts the assertion.
func makePodToVerifyCgroupsAbsent(cgroupNames []string) *v1.Pod {
	rootCgroupName := cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup)
	command := ""
	for _, baseName := range cgroupNames {
		cgroupComponents := strings.Split(baseName, "/")
		cgroupName := cm.NewCgroupName(rootCgroupName, cgroupComponents...)
		cgroupFsName := toCgroupFsName(cgroupName)
		if IsCgroup2UnifiedMode() {
			command += "if [ -d /tmp/" + cgroupFsName + " ]; then exit 1; fi; "
		} else {
			command += "if [ -d /tmp/memory/" + cgroupFsName + " ] || [ -d /tmp/cpu/" + cgroupFsName + " ]; then exit 1; fi; "
		}
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod" + string(uuid.NewUUID()),
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Image:   busyboxImage,
					Name:    "container" + string(uuid.NewUUID()),
					Command: []string{"sh", "-c", command},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "sysfscgroup",
							MountPath: "/tmp",
						},
					},
				},
			},
			Volumes: []v1.Volume{
				{
					Name: "sysfscgroup",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{Path: "/sys/fs/cgroup"},
					},
				},
			},
		},
	}
}

// makeSystemPartitionPod returns a pod with the requested resources. The
// namespace determines whether the pod is routed to the system partition.
func makeSystemPartitionPod(name, namespace string, requests, limits v1.ResourceList) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: v1.PodSpec{
			RestartPolicy: v1.RestartPolicyNever,
			Containers: []v1.Container{
				{
					Image:     imageutils.GetPauseImageName(),
					Name:      "container" + string(uuid.NewUUID()),
					Resources: getResourceRequirements(requests, limits),
				},
			},
		},
	}
}

// readCgroupInt64 reads a cgroup v2 file and returns its content as int64.
// It returns -1 for the literal "max".
func readCgroupInt64(cgroupPath, fileName string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(cgroupPath, fileName))
	if err != nil {
		return 0, err
	}
	val := strings.TrimSpace(string(data))
	if val == "max" {
		return -1, nil
	}
	return strconv.ParseInt(val, 10, 64)
}

var _ = SIGDescribe("System Partition", ginkgo.Ordered, framework.WithSerial(), feature.NodeSystemPartition, framework.WithFeatureGate(features.NodeSystemPartition), func() {
	f := framework.NewDefaultFramework("system-partition")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	var oldCfg *kubeletconfig.KubeletConfiguration

	ginkgo.BeforeEach(func(ctx context.Context) {
		if !IsCgroup2UnifiedMode() {
			ginkgo.Skip("System partition tests require cgroups v2")
		}
		if !kubeletCfg.CgroupsPerQOS {
			ginkgo.Skip("System partition tests require CgroupsPerQOS")
		}
		if oldCfg == nil {
			var err error
			oldCfg, err = getCurrentKubeletConfig(ctx)
			framework.ExpectNoError(err)
		}
	})

	ginkgo.AfterAll(func(ctx context.Context) {
		if oldCfg != nil {
			updateKubeletConfig(ctx, f, oldCfg, true)
		}
	})

	// configureSystemPartition restarts the kubelet with the NodeSystemPartition
	// feature enabled (or disabled) and the kube-system namespace mapped to the
	// system partition.
	configureSystemPartition := func(ctx context.Context, enabled bool) {
		newCfg := oldCfg.DeepCopy()
		if newCfg.FeatureGates == nil {
			newCfg.FeatureGates = make(map[string]bool)
		}
		newCfg.FeatureGates["NodeSystemPartition"] = enabled
		newCfg.CgroupsPerQOS = true
		if enabled {
			newCfg.SystemPartition = kubeletconfig.SystemPartitionConfiguration{
				MemoryLimit: resource.MustParse(systemPartitionMemoryLimit),
				CPUSet:      systemPartitionCPUSet,
				Namespaces:  []string{kubeapi.NamespaceSystem},
			}
		} else {
			newCfg.SystemPartition = kubeletconfig.SystemPartitionConfiguration{}
		}
		updateKubeletConfig(ctx, f, newCfg, true)
	}

	ginkgo.Context("when the feature is enabled", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			configureSystemPartition(ctx, true)
		})

		ginkgo.It("should create the system partition QoS cgroup hierarchy", func(ctx context.Context) {
			cgroupsToVerify := []string{"system", "system/" + burstableCgroup, "system/" + bestEffortCgroup}
			pod := makePodToVerifyCgroups(cgroupsToVerify)
			e2epod.NewPodClient(f).Create(ctx, pod)
			err := e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
			framework.ExpectNoError(err)
		})

		ginkgo.It("should apply the configured memory.max to the system partition cgroup", func(ctx context.Context) {
			got, err := readCgroupInt64(systemPartitionCgroupFsPath(), cgroupMemoryMax)
			framework.ExpectNoError(err, "reading system partition memory.max")
			wantQuantity := resource.MustParse(systemPartitionMemoryLimit)
			want := wantQuantity.Value()
			framework.Logf("system partition memory.max: got=%d, expected=%d", got, want)
			gomega.Expect(got).To(gomega.Equal(want), "system partition memory.max should match configured memoryLimit")
		})

		ginkgo.It("should apply the configured cpuset.cpus to the system partition cgroup", func(ctx context.Context) {
			data, err := os.ReadFile(filepath.Join(systemPartitionCgroupFsPath(), "cpuset.cpus"))
			framework.ExpectNoError(err, "reading system partition cpuset.cpus")
			got := strings.TrimSpace(string(data))
			framework.Logf("system partition cpuset.cpus: got=%q, expected=%q", got, systemPartitionCPUSet)
			gomega.Expect(got).To(gomega.Equal(systemPartitionCPUSet), "system partition cpuset.cpus should match configured cpuset")
		})

		ginkgo.It("should place a system namespace pod under the system partition", func(ctx context.Context) {
			pod := makeSystemPartitionPod("system-partition-guaranteed", kubeapi.NamespaceSystem,
				getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi"))
			pod = e2epod.PodClientNS(f, kubeapi.NamespaceSystem).CreateSync(ctx, pod)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				e2epod.PodClientNS(f, kubeapi.NamespaceSystem).DeleteSync(ctx, pod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			})

			podCgroup := systemPartitionCgroupFsPath("pod" + string(pod.UID))
			gomega.Eventually(ctx, func() bool {
				_, err := os.Stat(podCgroup)
				return err == nil
			}, f.Timeouts.PodStart, framework.Poll).Should(gomega.BeTrue(),
				fmt.Sprintf("expected system pod cgroup at %q", podCgroup))
		})

		ginkgo.It("should place a non-system namespace pod under the default partition", func(ctx context.Context) {
			pod := makeSystemPartitionPod("default-partition-guaranteed", f.Namespace.Name,
				getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi"))
			pod = e2epod.NewPodClient(f).CreateSync(ctx, pod)

			// The guaranteed pod cgroup for a default-partition pod lives directly
			// under kubepods, not under kubepods/system.
			kubepods := cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup)
			defaultCgroup := filepath.Join(cgroupRoot, toCgroupFsName(cm.NewCgroupName(kubepods, "pod"+string(pod.UID))))
			systemCgroup := systemPartitionCgroupFsPath("pod" + string(pod.UID))

			gomega.Eventually(ctx, func() bool {
				_, err := os.Stat(defaultCgroup)
				return err == nil
			}, f.Timeouts.PodStart, framework.Poll).Should(gomega.BeTrue(),
				fmt.Sprintf("expected default pod cgroup at %q", defaultCgroup))

			_, err := os.Stat(systemCgroup)
			gomega.Expect(os.IsNotExist(err)).To(gomega.BeTrue(),
				fmt.Sprintf("default namespace pod should not appear under the system partition at %q", systemCgroup))
		})
	})

	ginkgo.Context("when the feature is disabled", func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			configureSystemPartition(ctx, false)
		})

		ginkgo.It("should not create the system partition cgroup", func(ctx context.Context) {
			cgroupsToVerify := []string{"system"}
			pod := makePodToVerifyCgroupsAbsent(cgroupsToVerify)
			e2epod.NewPodClient(f).Create(ctx, pod)
			err := e2epod.WaitForPodSuccessInNamespace(ctx, f.ClientSet, pod.Name, f.Namespace.Name)
			framework.ExpectNoError(err)
		})
	})
})
