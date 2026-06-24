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
	"maps"

	"github.com/spf13/pflag"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kubernetes/cmd/kubelet/app/options"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

// kubeletFlagsConfigzGVK is the external GroupVersionKind stamped on the
// KubeletFlagsConfigz object so component-base/configz (which requires a
// runtime.Object carrying an external GVK) accepts it. It is not a real,
// registered API type; the GVK exists only to label the /configz entry.
var kubeletFlagsConfigzGVK = schema.GroupVersionKind{
	Group:   "kubelet.config.k8s.io",
	Version: "v1alpha1",
	Kind:    "KubeletFlags",
}

// KubeletFlagsConfigz is an exposable, JSON-serializable view of the kubelet
// command-line flags that are NOT merged into KubeletConfiguration (the fields
// of options.KubeletFlags). It is published under the "kubeletflags" key of the
// /configz endpoint.
//
// None of these fields carry secret *contents*: they are filesystem paths,
// identifiers, booleans, and labels — never the contents of secret files.
// KubeletConfiguration already exposes comparable paths (e.g. tlsPrivateKeyFile,
// authentication.x509.clientCAFile, staticPodPath) via /configz, so exposing
// these paths does not leak anything that is not already discoverable.
type KubeletFlagsConfigz struct {
	metav1.TypeMeta `json:",inline"`

	// KubeConfig is the path to a kubeconfig file (--kubeconfig). Path only.
	KubeConfig string `json:"kubeConfig,omitempty"`
	// BootstrapKubeconfig is the path to the bootstrap kubeconfig
	// (--bootstrap-kubeconfig). Path only.
	BootstrapKubeconfig string `json:"bootstrapKubeconfig,omitempty"`
	// HostnameOverride is the node identification override (--hostname-override).
	HostnameOverride string `json:"hostnameOverride,omitempty"`
	// NodeIP is the node IP address(es) (--node-ip).
	NodeIP string `json:"nodeIP,omitempty"`
	// CertDirectory is where the TLS certs live (--cert-dir). Path only.
	CertDirectory string `json:"certDirectory,omitempty"`
	// CloudProvider is the cloud services provider (--cloud-provider).
	CloudProvider string `json:"cloudProvider,omitempty"`
	// CloudConfigFile is the path to the cloud provider config (--cloud-config).
	// Path only.
	CloudConfigFile string `json:"cloudConfigFile,omitempty"`
	// RootDirectory is where the kubelet places its files (--root-dir). Path only.
	RootDirectory string `json:"rootDirectory,omitempty"`
	// KubeletConfigFile is the initial config file path (--config). Path only.
	KubeletConfigFile string `json:"kubeletConfigFile,omitempty"`
	// KubeletDropinConfigDirectory is the drop-in config directory (--config-dir).
	// Path only.
	KubeletDropinConfigDirectory string `json:"kubeletDropinConfigDirectory,omitempty"`
	// ExperimentalMounterPath is the path of the mounter binary
	// (--experimental-mounter-path). Path only.
	ExperimentalMounterPath string `json:"experimentalMounterPath,omitempty"`
	// ExperimentalNodeAllocatableIgnoreEvictionThreshold toggles ignoring hard
	// eviction limits in Node Allocatable (--experimental-allocatable-ignore-eviction).
	ExperimentalNodeAllocatableIgnoreEvictionThreshold bool `json:"experimentalNodeAllocatableIgnoreEvictionThreshold,omitempty"`
	// NodeLabels are labels added when registering the node (--node-labels).
	// These are operator-supplied identifiers, not secret material.
	NodeLabels map[string]string `json:"nodeLabels,omitempty"`
	// LockFilePath is the kubelet lock file path (--lock-file). Path only.
	LockFilePath string `json:"lockFilePath,omitempty"`
	// ExitOnLockContention toggles bootstrap lock-contention behavior
	// (--exit-on-lock-contention).
	ExitOnLockContention bool `json:"exitOnLockContention,omitempty"`
	// SeccompDefault enables RuntimeDefault as the default seccomp profile
	// (--seccomp-default).
	SeccompDefault bool `json:"seccompDefault,omitempty"`

	// Deprecated GC flags. Retained here so they are still visible while
	// deprecated. None carry secret content.

	// MinimumGCAge is the minimum age of a finished container before GC
	// (--minimum-container-ttl-duration).
	MinimumGCAge metav1.Duration `json:"minimumGCAge"`
	// MaxPerPodContainerCount is the per-container retention count
	// (--maximum-dead-containers-per-container).
	MaxPerPodContainerCount int32 `json:"maxPerPodContainerCount,omitempty"`
	// MaxContainerCount is the global container retention count
	// (--maximum-dead-containers).
	MaxContainerCount int32 `json:"maxContainerCount,omitempty"`

	// Embedded ContainerRuntimeOptions fields that are not merged into
	// KubeletConfiguration.

	// RuntimeCgroups is the cgroup the runtime is expected to run in
	// (--runtime-cgroups).
	RuntimeCgroups string `json:"runtimeCgroups,omitempty"`
	// ImageCredentialProviderConfigPath is the credential provider plugin config
	// file/dir (--image-credential-provider-config). Path only; the contents of
	// that file are published (with secrets redacted) under the separate
	// "credentialproviderconfig" /configz entry.
	ImageCredentialProviderConfigPath string `json:"imageCredentialProviderConfig,omitempty"`
	// ImageCredentialProviderBinDir is the credential provider plugin binary dir
	// (--image-credential-provider-bin-dir). Path only.
	ImageCredentialProviderBinDir string `json:"imageCredentialProviderBinDir,omitempty"`
}

// GetObjectKind implements runtime.Object via the embedded TypeMeta.
func (in *KubeletFlagsConfigz) GetObjectKind() schema.ObjectKind {
	return &in.TypeMeta
}

// DeepCopyObject implements runtime.Object.
func (in *KubeletFlagsConfigz) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(KubeletFlagsConfigz)
	*out = *in
	out.TypeMeta = in.TypeMeta
	if in.NodeLabels != nil {
		out.NodeLabels = make(map[string]string, len(in.NodeLabels))
		maps.Copy(out.NodeLabels, in.NodeLabels)
	}
	return out
}

// newKubeletFlagsConfigz maps an options.KubeletFlags into the exposable
// KubeletFlagsConfigz type and stamps it with the external GVK so configz.Set
// accepts it.
func newKubeletFlagsConfigz(f *options.KubeletFlags) *KubeletFlagsConfigz {
	cz := &KubeletFlagsConfigz{
		KubeConfig:                   f.KubeConfig,
		BootstrapKubeconfig:          f.BootstrapKubeconfig,
		HostnameOverride:             f.HostnameOverride,
		NodeIP:                       f.NodeIP,
		CertDirectory:                f.CertDirectory,
		CloudProvider:                f.CloudProvider,
		CloudConfigFile:              f.CloudConfigFile,
		RootDirectory:                f.RootDirectory,
		KubeletConfigFile:            f.KubeletConfigFile,
		KubeletDropinConfigDirectory: f.KubeletDropinConfigDirectory,
		ExperimentalMounterPath:      f.ExperimentalMounterPath,
		ExperimentalNodeAllocatableIgnoreEvictionThreshold: f.ExperimentalNodeAllocatableIgnoreEvictionThreshold,
		NodeLabels:                        f.NodeLabels,
		LockFilePath:                      f.LockFilePath,
		ExitOnLockContention:              f.ExitOnLockContention,
		SeccompDefault:                    f.SeccompDefault,
		MinimumGCAge:                      f.MinimumGCAge,
		MaxPerPodContainerCount:           f.MaxPerPodContainerCount,
		MaxContainerCount:                 f.MaxContainerCount,
		RuntimeCgroups:                    f.RuntimeCgroups,
		ImageCredentialProviderConfigPath: f.ImageCredentialProviderConfigPath,
		ImageCredentialProviderBinDir:     f.ImageCredentialProviderBinDir,
	}
	cz.GetObjectKind().SetGroupVersionKind(kubeletFlagsConfigzGVK)
	return cz
}

// nonMergedFlagNames returns the names of all command-line flags that are NOT
// merged into KubeletConfiguration, i.e. the flags that live only in
// options.KubeletFlags.
//
// It is computed universally by diffing the two flag sets that the kubelet
// registers — the KubeletFlags set (options.(*KubeletFlags).AddFlags) and the
// KubeletConfiguration set (options.AddKubeletConfigFlags) — rather than by
// maintaining a hand-curated list. This keeps the "is this flag exposed via the
// kubeletconfig entry already?" decision correct as flags are added or moved
// between the two sets over time. The companion unit test uses it to guard that
// KubeletFlagsConfigz exposes every non-merged flag.
func nonMergedFlagNames() sets.Set[string] {
	flagsFS := pflag.NewFlagSet("kubeletflags", pflag.ContinueOnError)
	options.NewKubeletFlags().AddFlags(flagsFS)

	configFS := pflag.NewFlagSet("kubeletconfig", pflag.ContinueOnError)
	options.AddKubeletConfigFlags(configFS, &kubeletconfiginternal.KubeletConfiguration{})

	names := sets.New[string]()
	flagsFS.VisitAll(func(fl *pflag.Flag) {
		// A flag that is also registered against KubeletConfiguration is already
		// visible through the existing "kubeletconfig" /configz entry, so it is
		// considered merged and excluded here.
		if configFS.Lookup(fl.Name) == nil {
			names.Insert(fl.Name)
		}
	})
	return names
}

// kubeletFlagsConfigzCoveredFlags is the set of non-merged command-line flag
// names that KubeletFlagsConfigz exposes. It is the authoritative registry that
// nonMergedFlagNames() is checked against in the unit tests: when a new
// non-merged flag is added to options.KubeletFlags, the guard test fails until
// the flag is added both to KubeletFlagsConfigz and to this set.
var kubeletFlagsConfigzCoveredFlags = sets.New[string](
	"kubeconfig",
	"bootstrap-kubeconfig",
	"hostname-override",
	"node-ip",
	"cert-dir",
	"cloud-provider",
	"root-dir",
	"config",
	"config-dir",
	"experimental-mounter-path",
	"experimental-allocatable-ignore-eviction",
	"node-labels",
	"lock-file",
	"exit-on-lock-contention",
	"seccomp-default",
	"minimum-container-ttl-duration",
	"maximum-dead-containers-per-container",
	"maximum-dead-containers",
	"runtime-cgroups",
	"image-credential-provider-config",
	"image-credential-provider-bin-dir",
)
