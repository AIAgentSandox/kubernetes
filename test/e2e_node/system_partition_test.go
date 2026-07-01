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
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	kubeapi "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/cm"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	evictionapi "k8s.io/kubernetes/pkg/kubelet/eviction/api"
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
	// systemPartitionEvictionThreshold is the memory.available hard eviction
	// threshold. The system partition reuses the node-level memory.available
	// threshold, applied relative to the partition memoryLimit. With a 1Gi
	// partition, the partition crosses the threshold once it uses roughly
	// (1Gi - threshold) bytes, which is well below memory.max, so eviction
	// fires before the kernel OOM killer.
	systemPartitionEvictionThreshold = "300Mi"
	// systemPartitionEvictionTimeout bounds how long we wait for the system
	// partition memhog pod to be evicted. The memhog fills the partition
	// slowly to avoid tripping the kernel OOM killer, so this is generous.
	systemPartitionEvictionTimeout = 15 * time.Minute
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

// makeSystemPartitionMemhogPod returns a memory-consuming pod placed in the
// given namespace. It reuses the shared memhog helper (agnhost stress) which
// allocates memory slowly enough that the eviction manager can react before the
// kernel OOM killer fires. No resource limits are set, so the pod fills the
// enclosing cgroup (the system partition memory.max) up to the point eviction
// intervenes.
func makeSystemPartitionMemhogPod(name, namespace string) *v1.Pod {
	pod := getMemhogPod(name, name, v1.ResourceRequirements{})
	pod.Namespace = namespace
	return pod
}

// waitForPodEvicted waits until the given pod reaches the Failed phase with the
// eviction reason. It fails the test if the pod's container was OOM killed,
// which indicates eviction did not fire before the kernel OOM killer.
func waitForPodEvicted(ctx context.Context, f *framework.Framework, namespace, name string, timeout time.Duration) {
	gomega.Eventually(ctx, func(ctx context.Context) error {
		pod, err := f.ClientSet.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled" {
				framework.Failf("pod %s/%s container was OOMKilled; eviction did not fire before the kernel OOM killer", namespace, name)
			}
		}
		if pod.Status.Phase == v1.PodFailed && pod.Status.Reason == eviction.Reason {
			return nil
		}
		return fmt.Errorf("pod %s/%s not yet evicted: phase=%q reason=%q", namespace, name, pod.Status.Phase, pod.Status.Reason)
	}, timeout, evictionPollInterval).Should(gomega.Succeed())
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

	// configureSystemPartitionWithEviction restarts the kubelet with the feature
	// enabled and a memory.available hard eviction threshold configured. The
	// partition reuses the node-level memory.available threshold, applied
	// relative to the partition memoryLimit. Because the node has far more memory
	// than the 1Gi partition, only the partition crosses the threshold, so
	// eviction is scoped to the system partition.
	configureSystemPartitionWithEviction := func(ctx context.Context) {
		newCfg := oldCfg.DeepCopy()
		if newCfg.FeatureGates == nil {
			newCfg.FeatureGates = make(map[string]bool)
		}
		newCfg.FeatureGates["NodeSystemPartition"] = true
		newCfg.CgroupsPerQOS = true
		newCfg.SystemPartition = kubeletconfig.SystemPartitionConfiguration{
			MemoryLimit: resource.MustParse(systemPartitionMemoryLimit),
			CPUSet:      systemPartitionCPUSet,
			Namespaces:  []string{kubeapi.NamespaceSystem},
		}
		newCfg.EvictionHard = map[string]string{string(evictionapi.SignalMemoryAvailable): systemPartitionEvictionThreshold}
		newCfg.EvictionMinimumReclaim = map[string]string{}
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

	ginkgo.Context("when the system partition is under memory pressure", framework.WithSlow(), framework.WithDisruptive(), func() {
		ginkgo.BeforeEach(func(ctx context.Context) {
			configureSystemPartitionWithEviction(ctx)
		})

		ginkgo.It("should evict a system-partition pod before the kernel OOM killer fires", func(ctx context.Context) {
			// The memhog fills the 1Gi partition slowly. The partition reuses the
			// node-level memory.available hard threshold, so once partition usage
			// approaches memory.max the eviction manager evicts the pod. Because the
			// threshold leaves headroom below memory.max, eviction must fire before a
			// kernel OOM kill.
			pod := makeSystemPartitionMemhogPod("system-partition-memhog", kubeapi.NamespaceSystem)
			pod = e2epod.PodClientNS(f, kubeapi.NamespaceSystem).Create(ctx, pod)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				e2epod.PodClientNS(f, kubeapi.NamespaceSystem).DeleteSync(ctx, pod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			})

			ginkgo.By("waiting for the system-partition pod to be evicted before a kernel OOM kill")
			waitForPodEvicted(ctx, f, kubeapi.NamespaceSystem, pod.Name, systemPartitionEvictionTimeout)
		})

		ginkgo.It("should evict only system-partition pods when partition memory pressure fires", func(ctx context.Context) {
			// A memhog in the system namespace drives the partition into memory
			// pressure. A pod in a non-system namespace consumes no partition memory
			// and must not be evicted by the partition-scoped signal.
			systemPod := makeSystemPartitionMemhogPod("system-partition-memhog", kubeapi.NamespaceSystem)
			systemPod = e2epod.PodClientNS(f, kubeapi.NamespaceSystem).Create(ctx, systemPod)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				e2epod.PodClientNS(f, kubeapi.NamespaceSystem).DeleteSync(ctx, systemPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			})

			defaultPod := makeSystemPartitionPod("default-partition-innocent", f.Namespace.Name,
				getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi"))
			defaultPod = e2epod.NewPodClient(f).CreateSync(ctx, defaultPod)

			ginkgo.By("waiting for the system-partition pod to be evicted")
			waitForPodEvicted(ctx, f, kubeapi.NamespaceSystem, systemPod.Name, systemPartitionEvictionTimeout)

			ginkgo.By("verifying the non-system-partition pod is not evicted by partition pressure")
			gomega.Consistently(ctx, func(ctx context.Context) error {
				got, err := f.ClientSet.CoreV1().Pods(f.Namespace.Name).Get(ctx, defaultPod.Name, metav1.GetOptions{})
				if err != nil {
					return err
				}
				if got.Status.Phase != v1.PodRunning {
					return fmt.Errorf("default-partition pod %q unexpectedly left Running: phase=%q reason=%q", defaultPod.Name, got.Status.Phase, got.Status.Reason)
				}
				return nil
			}, 30*time.Second, evictionPollInterval).Should(gomega.Succeed())
		})
	})

	ginkgo.Context("when toggling the feature", framework.WithSlow(), framework.WithDisruptive(), func() {
		ginkgo.It("should place system pods back under the default hierarchy after the feature is disabled", func(ctx context.Context) {
			kubepods := cm.NewCgroupName(cm.RootCgroupName, defaultNodeAllocatableCgroup)

			ginkgo.By("enabling the feature and placing a system pod under the system partition")
			configureSystemPartition(ctx, true)
			enabledPod := makeSystemPartitionPod("toggle-enabled", kubeapi.NamespaceSystem,
				getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi"))
			enabledPod = e2epod.PodClientNS(f, kubeapi.NamespaceSystem).CreateSync(ctx, enabledPod)

			systemCgroup := systemPartitionCgroupFsPath("pod" + string(enabledPod.UID))
			gomega.Eventually(ctx, func() bool {
				_, err := os.Stat(systemCgroup)
				return err == nil
			}, f.Timeouts.PodStart, framework.Poll).Should(gomega.BeTrue(),
				fmt.Sprintf("expected system pod cgroup at %q while feature enabled", systemCgroup))

			e2epod.PodClientNS(f, kubeapi.NamespaceSystem).DeleteSync(ctx, enabledPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)

			ginkgo.By("disabling the feature and restarting the kubelet")
			configureSystemPartition(ctx, false)

			ginkgo.By("verifying a new system pod is placed under the default hierarchy")
			disabledPod := makeSystemPartitionPod("toggle-disabled", kubeapi.NamespaceSystem,
				getResourceList("100m", "100Mi"), getResourceList("100m", "100Mi"))
			disabledPod = e2epod.PodClientNS(f, kubeapi.NamespaceSystem).CreateSync(ctx, disabledPod)
			ginkgo.DeferCleanup(func(ctx context.Context) {
				e2epod.PodClientNS(f, kubeapi.NamespaceSystem).DeleteSync(ctx, disabledPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
			})

			defaultCgroup := filepath.Join(cgroupRoot, toCgroupFsName(cm.NewCgroupName(kubepods, "pod"+string(disabledPod.UID))))
			gomega.Eventually(ctx, func() bool {
				_, err := os.Stat(defaultCgroup)
				return err == nil
			}, f.Timeouts.PodStart, framework.Poll).Should(gomega.BeTrue(),
				fmt.Sprintf("expected default pod cgroup at %q after feature disabled", defaultCgroup))

			ginkgo.By("verifying the system partition cgroup no longer exists")
			_, err := os.Stat(systemPartitionCgroupFsPath())
			gomega.Expect(os.IsNotExist(err)).To(gomega.BeTrue(),
				fmt.Sprintf("system partition cgroup should be cleaned up at %q after feature disabled", systemPartitionCgroupFsPath()))
		})
	})
})
