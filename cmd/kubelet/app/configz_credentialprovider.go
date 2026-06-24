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

	"k8s.io/component-base/configz"
	"k8s.io/klog/v2"
	credentialproviderconfigv1 "k8s.io/kubelet/config/v1"
	"k8s.io/kubernetes/pkg/credentialprovider/plugin"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
	kubeletscheme "k8s.io/kubernetes/pkg/kubelet/apis/config/scheme"
)

// redactedEnvValue is the placeholder substituted for non-empty credential
// provider env values before the config is published via /configz. The env field
// can hold static secret material injected into the plugin process, so it is
// treated as admin-controlled secret content and redacted.
const redactedEnvValue = "[REDACTED]"

// redactCredentialProviderConfig returns a deep copy of cfg with every non-empty
// providers[].env[].value replaced by redactedEnvValue. All other fields
// (provider name, matchImages, args, tokenAttributes, etc.) are preserved as-is.
func redactCredentialProviderConfig(cfg *kubeletconfiginternal.CredentialProviderConfig) *kubeletconfiginternal.CredentialProviderConfig {
	redacted := cfg.DeepCopy()
	for i := range redacted.Providers {
		for j := range redacted.Providers[i].Env {
			if redacted.Providers[i].Env[j].Value != "" {
				redacted.Providers[i].Env[j].Value = redactedEnvValue
			}
		}
	}
	return redacted
}

// setCredentialProviderConfigz converts the (already redacted) internal
// CredentialProviderConfig to its external versioned type, stamps the external
// GVK, and registers it with the given configz entry. It mirrors setConfigz.
func setCredentialProviderConfigz(cz *configz.Config, cfg *kubeletconfiginternal.CredentialProviderConfig) error {
	scheme, _, err := kubeletscheme.NewSchemeAndCodecs()
	if err != nil {
		return err
	}
	versioned := &credentialproviderconfigv1.CredentialProviderConfig{}
	if err := scheme.Convert(cfg, versioned, nil); err != nil {
		return err
	}
	versioned.GetObjectKind().SetGroupVersionKind(credentialproviderconfigv1.SchemeGroupVersion.WithKind("CredentialProviderConfig"))
	return cz.Set(versioned)
}

// initCredentialProviderConfigz registers the credential provider config loaded
// from --image-credential-provider-config under the "credentialproviderconfig"
// key of /configz, with secret env values redacted.
//
// When configPath is empty the entry is not registered, so /configz simply omits
// "credentialproviderconfig". A load/parse failure is logged but does not fail
// kubelet startup beyond existing behavior: the same config is loaded and
// validated again when the credential provider is initialized, which is where
// such errors are surfaced authoritatively.
func initCredentialProviderConfigz(ctx context.Context, configPath string) error {
	logger := klog.FromContext(ctx)
	if configPath == "" {
		return nil
	}

	cfg, err := plugin.GetCredentialProviderConfig(configPath)
	if err != nil {
		logger.Error(err, "Failed to load credential provider config for configz", "path", configPath)
		return nil
	}

	cz, err := configz.New("credentialproviderconfig")
	if err != nil {
		logger.Error(err, "Failed to register configz for credential provider config")
		return err
	}
	if err := setCredentialProviderConfigz(cz, redactCredentialProviderConfig(cfg)); err != nil {
		logger.Error(err, "Failed to register credential provider config")
		return err
	}
	return nil
}
