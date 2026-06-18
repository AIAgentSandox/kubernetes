/*
Copyright 2025 The Kubernetes Authors.

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

package allocation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/status"
	statustest "k8s.io/kubernetes/pkg/kubelet/status/testing"
	kubeletutil "k8s.io/kubernetes/pkg/kubelet/util"
	"k8s.io/kubernetes/test/utils/ktesting"
	clocktesting "k8s.io/utils/clock/testing"
)

// fakeAdmitHandler is a configurable admit handler used to drive deferral tests.
type fakeAdmitHandler struct {
	admit       bool
	deferResult bool
	reason      string
	message     string
	// seenOtherPods records the OtherPods UID set observed on each Admit call,
	// in call order, so tests can verify the peer set passed to admission.
	seenOtherPods [][]types.UID
}

func (h *fakeAdmitHandler) Admit(_ context.Context, attrs *lifecycle.PodAdmitAttributes) lifecycle.PodAdmitResult {
	otherUIDs := make([]types.UID, 0, len(attrs.OtherPods))
	for _, p := range attrs.OtherPods {
		otherUIDs = append(otherUIDs, p.UID)
	}
	h.seenOtherPods = append(h.seenOtherPods, otherUIDs)
	if h.admit {
		return lifecycle.PodAdmitResult{Admit: true}
	}
	return lifecycle.PodAdmitResult{
		Admit:   false,
		Defer:   h.deferResult,
		Reason:  h.reason,
		Message: h.message,
	}
}

type rejectedPod struct {
	pod     *v1.Pod
	reason  string
	message string
}

// deferTestFixture wires up an allocation manager with a controllable clock,
// a single configurable admit handler, and capture of synced/rejected pods.
type deferTestFixture struct {
	manager  *manager
	handler  *fakeAdmitHandler
	clock    *clocktesting.FakeClock
	synced   []*v1.Pod
	rejected []rejectedPod
}

func newDeferTestFixture(t *testing.T, pods ...*v1.Pod) *deferTestFixture {
	t.Helper()
	logger, _ := ktesting.NewTestContext(t)
	statusManager := status.NewManager(&fake.Clientset{}, kubepod.NewBasicPodManager(), &statustest.FakePodDeletionSafetyProvider{}, kubeletutil.NewPodStartupLatencyTracker())

	f := &deferTestFixture{
		handler: &fakeAdmitHandler{reason: "DeviceNotReady", message: "device not ready"},
		clock:   clocktesting.NewFakeClock(time.Now()),
	}

	getPodByUID := func(uid types.UID) (*v1.Pod, bool) {
		for _, p := range pods {
			if p.UID == uid {
				return p, true
			}
		}
		return nil, false
	}

	am := NewInMemoryManager(
		logger,
		statusManager,
		func(_ context.Context, pod *v1.Pod) { f.synced = append(f.synced, pod) },
		func() []*v1.Pod { return pods },
		getPodByUID,
		func(_ context.Context, pod *v1.Pod, reason, message string) {
			f.rejected = append(f.rejected, rejectedPod{pod: pod, reason: reason, message: message})
		},
		config.NewSourcesReady(func(_ sets.Set[string]) bool { return true }),
		record.NewFakeRecorder(20),
	)
	m := am.(*manager)
	m.clock = f.clock
	m.AddPodAdmitHandlers(lifecycle.PodAdmitHandlers{f.handler})
	f.manager = m
	return f
}

func makeDeferTestPod(uid string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			UID:       types.UID(uid),
			Name:      "pod-" + uid,
			Namespace: "default",
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{{Name: "c1"}},
		},
	}
}

// Pod deferred when handler returns Defer: true — not rejected, tracked with timestamp.
func TestAddPodDeferred(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-1")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	ok, deferred, reason, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.False(t, ok, "pod should not be admitted")
	require.True(t, deferred, "pod admission should be deferred")
	require.Equal(t, "DeviceNotReady", reason)

	f.manager.allocationMutex.Lock()
	firstSeen, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.True(t, tracked, "deferred pod should be tracked")
	require.Equal(t, f.clock.Now(), firstSeen, "first-seen time should be recorded")
	require.Empty(t, f.rejected, "deferred pod must not be rejected")
}

// AddPod called repeatedly preserves the original first-seen time.
func TestAddPodDeferredPreservesFirstSeenTime(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-2")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)
	firstSeen := f.manager.podsWithDeferredAdmission[pod.UID]

	f.clock.Step(30 * time.Second)
	_, deferred, _, _ = f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)
	require.Equal(t, firstSeen, f.manager.podsWithDeferredAdmission[pod.UID], "first-seen time must be preserved across re-adds")
}

// Retry succeeds when device becomes available — pod admitted and synced.
func TestRetryDeferredAdmissionSucceeds(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-3")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	// Device becomes available.
	f.handler.admit = true
	f.clock.Step(10 * time.Second)
	f.manager.RetryDeferredAdmissions(tCtx)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "admitted pod should be removed from deferred map")
	require.Len(t, f.synced, 1, "admitted pod should be synced")
	require.Equal(t, pod.UID, f.synced[0].UID)
	require.Empty(t, f.rejected, "successfully admitted pod must not be rejected")
}

// Retry keeps deferring while still within the timeout and the device is unavailable.
func TestRetryDeferredAdmissionStillDeferred(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-4")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	f.clock.Step(30 * time.Second) // within the 1 minute timeout
	f.manager.RetryDeferredAdmissions(tCtx)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.True(t, tracked, "pod should still be deferred within the timeout")
	require.Empty(t, f.rejected, "pod within timeout must not be rejected")
	require.Empty(t, f.synced)
}

// Retry fails after timeout — pod rejected with the rejection callback.
func TestRetryDeferredAdmissionTimesOut(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-5")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	// Advance past the deferral timeout; device is still unavailable.
	f.clock.Step(deferredAdmissionTimeout + time.Second)
	f.manager.RetryDeferredAdmissions(tCtx)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "timed-out pod should be removed from deferred map")
	require.Len(t, f.rejected, 1, "timed-out pod should be rejected")
	require.Equal(t, pod.UID, f.rejected[0].pod.UID)
	require.Empty(t, f.synced)
}

// Pod removed while deferred — cleaned up properly.
func TestRemovePodCleansUpDeferred(t *testing.T) {
	tCtx := ktesting.Init(t)
	logger, _ := ktesting.NewTestContext(t)
	pod := makeDeferTestPod("defer-6")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	f.manager.RemovePod(logger, pod.UID)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "removed pod should be cleared from deferred map")
}

// RemoveOrphanedPods cleans up deferred pods no longer in the remaining set.
func TestRemoveOrphanedPodsCleansUpDeferred(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-7")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	// Remaining set does not contain the deferred pod.
	f.manager.RemoveOrphanedPods(sets.New[types.UID]("some-other-pod"))

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "orphaned deferred pod should be cleared")
}

// With multiple pods deferred, each retry must see the correct OtherPods set.
// Regression test for the shared allocatedPods slice being mutated in place
// across iterations of the retry loop (which would leave later pods with an
// empty/corrupted peer set).
func TestRetryDeferredAdmissionMultiplePodsOtherPods(t *testing.T) {
	tCtx := ktesting.Init(t)
	p1 := makeDeferTestPod("multi-1")
	p2 := makeDeferTestPod("multi-2")
	f := newDeferTestFixture(t, p1, p2)
	f.handler.admit = false
	f.handler.deferResult = true

	_, d1, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{p1, p2}, p1)
	require.True(t, d1)
	_, d2, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{p1, p2}, p2)
	require.True(t, d2)

	f.handler.seenOtherPods = nil
	f.manager.RetryDeferredAdmissions(tCtx)

	require.Len(t, f.handler.seenOtherPods, 2, "both deferred pods should be re-evaluated")
	for _, seen := range f.handler.seenOtherPods {
		require.Len(t, seen, 1, "each pod should see exactly the one other active pod as OtherPods")
	}
}

// A previously-deferred pod that later fails for a non-deferrable reason while
// still inside the timeout window is rejected with that reason, not relabeled
// as a deferral timeout.
func TestRetryDeferredAdmissionNonDeferrableRejection(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-nondef")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	// Within the timeout, admission now fails for a different, non-deferrable reason.
	f.handler.deferResult = false
	f.handler.reason = "OutOfMemory"
	f.handler.message = "node out of memory"
	f.clock.Step(10 * time.Second)
	f.manager.RetryDeferredAdmissions(tCtx)

	require.Len(t, f.rejected, 1, "non-deferrable failure should reject the pod")
	require.Equal(t, "OutOfMemory", f.rejected[0].reason, "reason must not be relabeled as a timeout")
	require.Equal(t, "node out of memory", f.rejected[0].message, "message must not be prefixed with timeout text")

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "rejected pod should be removed from the deferred map")
}

// A pod deleted between deferral and retry is silently dropped, not synced or rejected.
func TestRetryDeferredAdmissionPodDeleted(t *testing.T) {
	tCtx := ktesting.Init(t)
	f := newDeferTestFixture(t)

	f.manager.allocationMutex.Lock()
	f.manager.podsWithDeferredAdmission["ghost-uid"] = f.clock.Now()
	f.manager.allocationMutex.Unlock()

	f.manager.RetryDeferredAdmissions(tCtx)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission["ghost-uid"]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "deleted pod should be dropped from the deferred map")
	require.Empty(t, f.synced, "deleted pod must not be synced")
	require.Empty(t, f.rejected, "deleted pod must not be rejected")
}

// At exactly the timeout boundary the pod is still deferred (the check is <=).
func TestRetryDeferredAdmissionAtTimeoutBoundary(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("defer-boundary")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = true

	_, deferred, _, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.True(t, deferred)

	f.clock.Step(deferredAdmissionTimeout) // exactly at the boundary
	f.manager.RetryDeferredAdmissions(tCtx)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.True(t, tracked, "pod at exactly the timeout boundary should still be deferred")
	require.Empty(t, f.rejected, "pod at the boundary must not be rejected")
}

// Non-device-plugin rejections still reject immediately (no deferral).
func TestAddPodNonDeferrableRejection(t *testing.T) {
	tCtx := ktesting.Init(t)
	pod := makeDeferTestPod("reject-1")
	f := newDeferTestFixture(t, pod)
	f.handler.admit = false
	f.handler.deferResult = false
	f.handler.reason = "OutOfMemory"

	ok, deferred, reason, _ := f.manager.AddPod(tCtx, []*v1.Pod{pod}, pod)
	require.False(t, ok, "pod should not be admitted")
	require.False(t, deferred, "non-deferrable rejection must not defer")
	require.Equal(t, "OutOfMemory", reason)

	f.manager.allocationMutex.Lock()
	_, tracked := f.manager.podsWithDeferredAdmission[pod.UID]
	f.manager.allocationMutex.Unlock()
	require.False(t, tracked, "non-deferrable rejection must not be tracked")
}
