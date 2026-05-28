//go:build linux

/*
Copyright 2026 The Kubernetes Authors.

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

// Run locally:
//
//	make test-e2e-node \
//	    FOCUS='Pod InPlace Resize \(node\).*should resize CPU and memory' \
//	    TEST_ARGS='--kubelet-flags="--fail-swap-on=false"'

package e2enode

import (
	"context"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kubernetes/test/e2e/common/node/framework/cgroups"
	"k8s.io/kubernetes/test/e2e/common/node/framework/podresize"
	"k8s.io/kubernetes/test/e2e/feature"
	"k8s.io/kubernetes/test/e2e/framework"
	e2epod "k8s.io/kubernetes/test/e2e/framework/pod"
	e2eskipper "k8s.io/kubernetes/test/e2e/framework/skipper"
	admissionapi "k8s.io/pod-security-admission/api"

	"github.com/onsi/ginkgo/v2"
)

var _ = SIGDescribe("Pod InPlace Resize (node)", framework.WithSerial(), feature.InPlacePodVerticalScaling, func() {
	f := framework.NewDefaultFramework("pod-resize-node-tests")
	f.NamespacePodSecurityLevel = admissionapi.LevelBaseline

	ginkgo.It("should resize CPU and memory of a guaranteed pod in place", func(ctx context.Context) {
		podClient := e2epod.NewPodClient(f)

		originalContainers := []podresize.ResizableContainerInfo{
			{
				Name: "c1",
				Resources: &cgroups.ContainerResources{
					CPUReq: "100m", CPULim: "100m",
					MemReq: "64Mi", MemLim: "64Mi",
				},
			},
		}
		expectedContainers := []podresize.ResizableContainerInfo{
			{
				Name: "c1",
				Resources: &cgroups.ContainerResources{
					CPUReq: "200m", CPULim: "200m",
					MemReq: "128Mi", MemLim: "128Mi",
				},
			},
		}

		ginkgo.By("creating a guaranteed pod")
		tStamp := strconv.Itoa(time.Now().Nanosecond())
		testPod := podresize.MakePodWithResizableContainers(f.Namespace.Name, "", tStamp, originalContainers, nil)
		testPod.GenerateName = "resize-node-test-"

		newPod := podClient.CreateSync(ctx, testPod)

		if !cgroups.IsPodOnCgroupv2Node(f, newPod.Name, newPod.Spec.Containers[0].Name) {
			e2eskipper.Skipf("cgroup v2 required for in-place resize cgroup verification")
		}

		ginkgo.By("verifying initial pod resources, status, and container cgroup values")
		podresize.VerifyPodResources(newPod, originalContainers, nil)
		podresize.VerifyPodResizePolicy(newPod, originalContainers)
		framework.ExpectNoError(podresize.VerifyPodStatusResources(newPod, originalContainers))
		framework.ExpectNoError(podresize.VerifyPodContainersCgroupValues(ctx, f, newPod, originalContainers))

		ginkgo.By("patching pod via resize subresource to increase CPU and memory")
		patch := podresize.MakeResizePatch(originalContainers, expectedContainers, nil, nil)
		patchedPod, err := f.ClientSet.CoreV1().Pods(newPod.Namespace).Patch(
			ctx, newPod.Name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}, "resize")
		framework.ExpectNoError(err, "failed to patch pod for resize")

		expected := podresize.UpdateExpectedContainerRestarts(ctx, patchedPod, expectedContainers)
		podresize.VerifyPodResources(patchedPod, expected, nil)

		ginkgo.By("waiting for the resize to be actuated by the kubelet")
		resizedPod := podresize.WaitForPodResizeActuation(ctx, f, podClient, newPod, expected)

		ginkgo.By("verifying resized pod resources, status, and container cgroup values")
		podresize.ExpectPodResized(ctx, f, resizedPod, expected)
		podresize.VerifyPodResources(resizedPod, expected, nil)
		framework.ExpectNoError(podresize.VerifyPodContainersCgroupValues(ctx, f, resizedPod, expected))

		ginkgo.By("deleting pod")
		podClient.DeleteSync(ctx, newPod.Name, metav1.DeleteOptions{}, f.Timeouts.PodDelete)
	})
})
