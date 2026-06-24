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

package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/configz"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
)

func populatedKubeletFlags() *options.KubeletFlags {
	f := options.NewKubeletFlags()
	f.KubeConfig = "/etc/kubernetes/kubelet.conf"
	f.BootstrapKubeconfig = "/etc/kubernetes/bootstrap-kubelet.conf"
	f.HostnameOverride = "node-1"
	f.NodeIP = "10.0.0.1"
	f.CertDirectory = "/var/lib/kubelet/pki"
	f.CloudProvider = "external"
	f.CloudConfigFile = "/etc/kubernetes/cloud.conf"
	f.RootDirectory = "/var/lib/kubelet"
	f.KubeletConfigFile = "/var/lib/kubelet/config.yaml"
	f.KubeletDropinConfigDirectory = "/etc/kubernetes/kubelet.conf.d"
	f.ExperimentalMounterPath = "/usr/bin/mounter"
	f.ExperimentalNodeAllocatableIgnoreEvictionThreshold = true
	f.NodeLabels = map[string]string{"example.com/role": "worker"}
	f.LockFilePath = "/var/run/kubelet.lock"
	f.ExitOnLockContention = true
	f.SeccompDefault = true
	f.MinimumGCAge = metav1.Duration{Duration: 0}
	f.MaxPerPodContainerCount = 2
	f.MaxContainerCount = 5
	f.RuntimeCgroups = "/systemd/system.slice"
	f.ImageCredentialProviderConfigPath = "/etc/kubernetes/credential-provider-config.yaml"
	f.ImageCredentialProviderBinDir = "/usr/libexec/kubelet/credential-providers"
	return f
}

func TestNewKubeletFlagsConfigz(t *testing.T) {
	f := populatedKubeletFlags()
	cz := newKubeletFlagsConfigz(f)

	require.Equal(t, kubeletFlagsConfigzGVK, cz.GetObjectKind().GroupVersionKind())
	require.Equal(t, "KubeletFlags", cz.Kind)
	require.Equal(t, "kubelet.config.k8s.io/v1alpha1", cz.APIVersion)

	require.Equal(t, f.RootDirectory, cz.RootDirectory)
	require.Equal(t, f.ImageCredentialProviderConfigPath, cz.ImageCredentialProviderConfigPath)
	require.Equal(t, f.ImageCredentialProviderBinDir, cz.ImageCredentialProviderBinDir)
	require.Equal(t, f.NodeLabels, cz.NodeLabels)
	require.True(t, cz.ExitOnLockContention)
	require.Equal(t, int32(5), cz.MaxContainerCount)
}

func TestKubeletFlagsConfigzDeepCopyObject(t *testing.T) {
	cz := newKubeletFlagsConfigz(populatedKubeletFlags())
	out, ok := cz.DeepCopyObject().(*KubeletFlagsConfigz)
	require.True(t, ok)
	require.Equal(t, cz, out)

	// Mutating the copy's map must not affect the original.
	out.NodeLabels["example.com/role"] = "control-plane"
	require.Equal(t, "worker", cz.NodeLabels["example.com/role"])
}

// TestInitFlagsConfigz exercises the full configz registration path and asserts
// the JSON served by the /configz handler is keyed "kubeletflags", carries the
// expected apiVersion/kind, and round-trips representative fields.
func TestInitFlagsConfigz(t *testing.T) {
	const name = "kubeletflags"
	// Ensure a clean slate and clean up after the test, since configz uses a
	// process-global registry.
	configz.Delete(name)
	t.Cleanup(func() { configz.Delete(name) })

	f := populatedKubeletFlags()
	require.NoError(t, initFlagsConfigz(context.Background(), f))

	mux := http.NewServeMux()
	configz.InstallHandler(mux)
	req := httptest.NewRequest(http.MethodGet, configz.DefaultConfigzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload map[string]map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))

	entry, ok := payload[name]
	require.True(t, ok, "expected /configz JSON to contain key %q", name)

	require.Equal(t, "kubelet.config.k8s.io/v1alpha1", entry["apiVersion"])
	require.Equal(t, "KubeletFlags", entry["kind"])
	require.Equal(t, f.ImageCredentialProviderConfigPath, entry["imageCredentialProviderConfig"])
	require.Equal(t, f.RootDirectory, entry["rootDirectory"])
	require.Equal(t, f.KubeConfig, entry["kubeConfig"])
}

// TestKubeletFlagsConfigzCoversAllNonMergedFlags is the guard for the universal
// merged-vs-not detection: it asserts that the set of non-merged flags computed
// by diffing the kubelet's flag sets exactly matches the set KubeletFlagsConfigz
// is declared to expose. If a new non-merged flag is added to options.KubeletFlags
// without being surfaced here, this test fails.
func TestKubeletFlagsConfigzCoversAllNonMergedFlags(t *testing.T) {
	nonMerged := nonMergedFlagNames()

	missing := nonMerged.Difference(kubeletFlagsConfigzCoveredFlags)
	require.Empty(t, missing.UnsortedList(),
		"non-merged kubelet flags not exposed via KubeletFlagsConfigz; add them to the type and to kubeletFlagsConfigzCoveredFlags")

	extra := kubeletFlagsConfigzCoveredFlags.Difference(nonMerged)
	require.Empty(t, extra.UnsortedList(),
		"kubeletFlagsConfigzCoveredFlags lists flags that are not (or no longer) non-merged kubelet flags")
}
