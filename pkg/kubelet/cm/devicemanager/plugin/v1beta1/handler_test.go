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

// Invariants enforced by the tests in this file
// (derived from handler.go, see Task 1 of the same-device-same-port plan):
//
//   I-S1 registerClient appends Client instances to s.clients[name] without
//        de-duplication; the per-resource slice can therefore hold multiple
//        clients with different SocketPath() values for the same name.
//        (handler.go:108-114)
//
//   I-S2 deregisterClient removes ONLY entries whose Client.SocketPath()
//        matches the supplied socketPath; other clients under the same name
//        survive. (handler.go:116-131)
//
//   I-S3 deregisterClient deletes the s.clients[name] map entry entirely
//        once the residual slice would be empty — readers must therefore
//        treat a missing key and an empty slice identically.
//        (handler.go:133-137)
//
//   I-S4 deregisterClient is a no-op when no entry under s.clients[name]
//        matches the socketPath (including the case where the whole key is
//        absent). It must not panic and must not touch unrelated clients.
//        (handler.go:124-131 — the loop body simply never matches.)
//
//   I-S5 getClient returns nil when no client with that (name, socketPath)
//        pair has been registered. (handler.go:154-163)
//
//   I-S6 Eviction in deregisterClient is keyed by socket path ALONE — there
//        is no identity check against the originally-registered *Client*
//        pointer. A late callback for an old client at socketA will evict
//        any client currently registered at socketA, regardless of whether
//        that is a different concrete *Client* instance. This is the
//        regression risk called out in the plan; see
//        TestServer_LateDisconnectDoesNotEvictNewClient for the
//        documenting/locking-in test.
//
// These tests are deliberately constructed against the unexported `server`
// type via a package-internal test file (we are in `package v1beta1`) so
// they can drive registerClient / deregisterClient / getClient directly
// without bringing up any gRPC machinery.

package v1beta1

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"
)

// fakeClient is a minimal Client implementation: Connect / Run / Disconnect
// are no-ops and SocketPath() returns whatever was configured. The
// registerClient / deregisterClient / getClient paths only ever read
// SocketPath(), so the rest of the interface can stay inert.
type fakeClient struct {
	socket string
}

func newFakeClient(socket string) *fakeClient {
	return &fakeClient{socket: socket}
}

func (f *fakeClient) Connect(_ context.Context) error { return nil }
func (f *fakeClient) Run(_ context.Context)           {}
func (f *fakeClient) Disconnect(_ klog.Logger) error  { return nil }
func (f *fakeClient) SocketPath() string              { return f.socket }

// newTestServer builds a *server literal sufficient to exercise the
// register / deregister / get paths. We don't need socketName, socketDir,
// grpc, rhandler or chandler because none of the tested methods touch them.
func newTestServer() *server {
	return &server{
		clients: make(map[string][]Client),
	}
}

// testLogger returns a discard logger appropriate for tests that don't
// care about logged output (the handler functions log at V(2)+).
func testLogger() klog.Logger {
	return klog.NewKlogr()
}

// socket paths used across tests. Absolute paths mirror the values that
// production code would produce via filepath.Join(socketDir, endpoint).
const (
	testPluginName = "domain1.com/resource1"
	otherName      = "domain1.com/resource2"
	socketA        = "/var/lib/kubelet/plugins/socketA.sock"
	socketB        = "/var/lib/kubelet/plugins/socketB.sock"
	socketC        = "/var/lib/kubelet/plugins/socketC.sock"
)

// TestServer_RegisterClient_AppendsPerSocket verifies I-S1: two clients
// registered under the same plugin name but at distinct socket paths must
// both live in the per-resource slice.
func TestServer_RegisterClient_AppendsPerSocket(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	cA := newFakeClient(socketA)
	cB := newFakeClient(socketB)

	s.registerClient(logger, testPluginName, cA)
	s.registerClient(logger, testPluginName, cB)

	require.Len(t, s.clients[testPluginName], 2,
		"two registerClient calls with the same name but different sockets must produce two entries (I-S1)")

	got := map[string]bool{}
	for _, c := range s.clients[testPluginName] {
		got[c.SocketPath()] = true
	}
	require.True(t, got[socketA], "socketA client must be retained (I-S1)")
	require.True(t, got[socketB], "socketB client must be retained (I-S1)")

	// Pointer identity check: registerClient must not wrap or copy the
	// passed Client — getClient should return the very same instance.
	require.Same(t, Client(cA), s.getClient(testPluginName, socketA),
		"getClient must return the exact registered Client instance (I-S1)")
	require.Same(t, Client(cB), s.getClient(testPluginName, socketB),
		"getClient must return the exact registered Client instance (I-S1)")
}

// TestServer_DeregisterClient_OnlyRemovesMatchingSocket verifies I-S2: a
// deregister keyed by socket path must spare other clients under the same
// plugin name.
func TestServer_DeregisterClient_OnlyRemovesMatchingSocket(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	cA := newFakeClient(socketA)
	cB := newFakeClient(socketB)
	s.registerClient(logger, testPluginName, cA)
	s.registerClient(logger, testPluginName, cB)
	require.Len(t, s.clients[testPluginName], 2, "test setup: both clients registered")

	s.deregisterClient(logger, testPluginName, socketA)

	require.Len(t, s.clients[testPluginName], 1,
		"deregisterClient must remove exactly one entry (I-S2)")
	require.Equal(t, socketB, s.clients[testPluginName][0].SocketPath(),
		"the surviving client must be the one at the non-matching socket (I-S2)")
	require.Same(t, Client(cB), s.clients[testPluginName][0],
		"the surviving client must be the exact original instance (I-S2)")

	// And: the survivor remains reachable via getClient.
	require.Same(t, Client(cB), s.getClient(testPluginName, socketB),
		"survivor must still be reachable via getClient (I-S2)")
	require.Nil(t, s.getClient(testPluginName, socketA),
		"the deregistered socket must no longer be reachable (I-S2)")
}

// TestServer_DeregisterClient_DeletesKeyWhenLast verifies I-S3:
// deregistering the only client must remove the map key entirely, not just
// leave behind an empty slice. Readers that test `_, ok := s.clients[name]`
// rely on this.
func TestServer_DeregisterClient_DeletesKeyWhenLast(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	c := newFakeClient(socketA)
	s.registerClient(logger, testPluginName, c)
	require.Contains(t, s.clients, testPluginName, "test setup: key present")

	s.deregisterClient(logger, testPluginName, socketA)

	_, present := s.clients[testPluginName]
	require.False(t, present,
		"after deregistering the last client, the map key must be deleted, not left as an empty slice (I-S3)")
	require.Nil(t, s.getClient(testPluginName, socketA),
		"after key deletion, getClient must return nil (I-S3)")
}

// TestServer_GetClient_ReturnsNilWhenSocketMissing verifies I-S5: looking
// up a socket that was never registered (with another socket registered
// under the same name) returns nil rather than panicking or returning the
// wrong client.
func TestServer_GetClient_ReturnsNilWhenSocketMissing(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	c := newFakeClient(socketA)
	s.registerClient(logger, testPluginName, c)

	require.Nil(t, s.getClient(testPluginName, socketB),
		"getClient must return nil when no entry matches the requested socket (I-S5)")
	require.Nil(t, s.getClient(otherName, socketA),
		"getClient must return nil when the requested name is unknown (I-S5)")
	// Sanity: the registered client is still found.
	require.Same(t, Client(c), s.getClient(testPluginName, socketA),
		"the registered (name, socket) must still be reachable (I-S5)")
}

// TestServer_DeregisterClient_NoopForUnknownSocket verifies I-S4:
// deregistering a socket that was never registered under that name must
// neither panic nor touch unrelated entries.
func TestServer_DeregisterClient_NoopForUnknownSocket(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	c := newFakeClient(socketA)
	s.registerClient(logger, testPluginName, c)

	// (a) Unknown socket under a known name: existing client must survive.
	require.NotPanics(t, func() {
		s.deregisterClient(logger, testPluginName, socketB)
	}, "deregister for an unknown socket under a known name must not panic (I-S4)")
	require.Len(t, s.clients[testPluginName], 1,
		"existing client must be untouched after deregister of an unknown socket (I-S4)")
	require.Same(t, Client(c), s.clients[testPluginName][0],
		"existing client identity must be preserved (I-S4)")

	// (b) Unknown name entirely: must also be a clean no-op.
	require.NotPanics(t, func() {
		s.deregisterClient(logger, otherName, socketA)
	}, "deregister for an unknown name must not panic (I-S4)")
	require.Len(t, s.clients[testPluginName], 1,
		"deregister for an unknown name must not perturb other names' entries (I-S4)")
	_, present := s.clients[otherName]
	require.False(t, present,
		"deregister for an unknown name must not create a phantom map entry (I-S4)")
}

// TestServer_LateDisconnectDoesNotEvictNewClient is the **core race test**.
//
// Scenario (mirrors the plan's same-device-same-port case): an old client
// at socketA has been deregistered; a brand-new client process re-registers
// at the *same* socketA; then a stale, late disconnect callback for the
// old client fires.
//
// Current production behavior (locked in by this test, see I-S6): the late
// callback evicts the new client because eviction in deregisterClient is
// keyed by socket path alone, not by Client identity (handler.go:124-131).
//
// This is the regression risk the plan calls out in its Questions section.
// If a future change introduces identity-aware eviction (Task 4 Option B),
// this test must flip together with the manager-side test
// TestSameSocketRace_LateDisconnectAfterReconnect.
func TestServer_LateDisconnectDoesNotEvictNewClient(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	c1 := newFakeClient(socketA)
	c2 := newFakeClient(socketA)
	require.NotSame(t, c1, c2,
		"sanity: c1 and c2 are deliberately distinct *fakeClient instances at the same path")

	// Step 1: register the original client at socketA.
	s.registerClient(logger, testPluginName, c1)
	require.Same(t, Client(c1), s.getClient(testPluginName, socketA),
		"c1 must be the registered client at socketA")

	// Step 2: deregister c1 cleanly.
	s.deregisterClient(logger, testPluginName, socketA)
	require.Nil(t, s.getClient(testPluginName, socketA),
		"c1 must be gone after clean deregister")

	// Step 3: a fresh process registers at the SAME socketA.
	s.registerClient(logger, testPluginName, c2)
	require.Same(t, Client(c2), s.getClient(testPluginName, socketA),
		"c2 must now be the registered client at socketA")
	require.Len(t, s.clients[testPluginName], 1,
		"exactly one client must be registered at socketA after the fresh register")

	// Step 4: a stale, late disconnect callback for c1 arrives.
	// Locked-in current behavior (I-S6): this evicts c2 even though c2 !=
	// c1, because eviction is keyed by socket path only.
	s.deregisterClient(logger, testPluginName, socketA)

	require.Nil(t, s.getClient(testPluginName, socketA),
		"current behavior: late callback evicts c2 because eviction is keyed by socket path only (I-S6 regression risk)")
	_, present := s.clients[testPluginName]
	require.False(t, present,
		"current behavior: socketA was the last entry, so the map key is removed when c2 is evicted (I-S6 + I-S3)")
}

// TestServer_ConcurrentRegisterDeregister exercises the server mutex by
// driving overlapping register / deregister calls from goroutines under
// `go test -race`. Beyond catching data races, it asserts that the final
// state is consistent: every client that was last-registered (and not
// subsequently deregistered) is still reachable via getClient, and every
// pair that was deregistered after registration is gone.
//
// This is not strictly required by the plan's task list, but it falls out
// of "Run with -race" (Task 6) and gives the per-socket-map a stress check
// alongside the existing manager-side TestSameSocketRace_OverlappingConnects.
func TestServer_ConcurrentRegisterDeregister(t *testing.T) {
	s := newTestServer()
	logger := testLogger()

	// Pre-register a baseline client that the concurrent workers must not
	// evict — it lives at a unique socket path.
	baseline := newFakeClient(socketC)
	s.registerClient(logger, testPluginName, baseline)

	// Worker A: repeatedly register/deregister at socketA.
	// Worker B: repeatedly register/deregister at socketB.
	// At the end, only `baseline` is guaranteed to still be present.
	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range iters {
			s.registerClient(logger, testPluginName, newFakeClient(socketA))
			s.deregisterClient(logger, testPluginName, socketA)
		}
	}()
	go func() {
		defer wg.Done()
		for range iters {
			s.registerClient(logger, testPluginName, newFakeClient(socketB))
			s.deregisterClient(logger, testPluginName, socketB)
		}
	}()
	wg.Wait()

	// The baseline must still be there: its socket is socketC and the
	// workers only ever touched socketA / socketB.
	require.Same(t, Client(baseline), s.getClient(testPluginName, socketC),
		"baseline client at socketC must survive concurrent register/deregister churn at other sockets")
	require.Nil(t, s.getClient(testPluginName, socketA),
		"every register at socketA was paired with a deregister; nothing should remain")
	require.Nil(t, s.getClient(testPluginName, socketB),
		"every register at socketB was paired with a deregister; nothing should remain")
}
