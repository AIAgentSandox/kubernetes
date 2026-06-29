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

package plugin

import (
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	configv1 "k8s.io/kubelet/config/v1"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

func TestGetRedactedCredentialProviderConfig(t *testing.T) {
	testcases := []struct {
		name     string
		config   *kubeletconfig.CredentialProviderConfig
		validate func(t *testing.T, cfg *configv1.CredentialProviderConfig)
	}{
		{
			name: "returns nil when no config is cached",
		},
		{
			name: "env values are redacted but names preserved",
			config: &kubeletconfig.CredentialProviderConfig{
				Providers: []kubeletconfig.CredentialProvider{{
					Name:                 "test",
					MatchImages:          []string{"registry.io/foobar"},
					DefaultCacheDuration: &metav1.Duration{},
					APIVersion:           "credentialprovider.kubelet.k8s.io/v1",
					Args:                 []string{"--v=5"},
					Env: []kubeletconfig.ExecEnvVar{
						{Name: "AWS_SECRET_ACCESS_KEY", Value: "super-secret-value"},
						{Name: "EMPTY", Value: ""},
					},
				}},
			},
			validate: func(t *testing.T, cfg *configv1.CredentialProviderConfig) {
				if len(cfg.Providers) != 1 {
					t.Fatalf("expected 1 provider, got %d", len(cfg.Providers))
				}
				p := cfg.Providers[0]
				// Non-sensitive fields must be preserved as-is.
				if p.Name != "test" {
					t.Errorf("expected provider name preserved, got %q", p.Name)
				}
				if len(p.MatchImages) != 1 || p.MatchImages[0] != "registry.io/foobar" {
					t.Errorf("expected matchImages preserved, got %v", p.MatchImages)
				}
				// Args are redacted to a single placeholder when present.
				if len(p.Args) != 1 || p.Args[0] != redactedValue {
					t.Errorf("expected args redacted to [%q], got %v", redactedValue, p.Args)
				}
				if len(p.Env) != 2 {
					t.Fatalf("expected 2 env vars, got %d", len(p.Env))
				}
				// Env name preserved, non-empty value redacted.
				if p.Env[0].Name != "AWS_SECRET_ACCESS_KEY" {
					t.Errorf("expected env name preserved, got %q", p.Env[0].Name)
				}
				if p.Env[0].Value != redactedValue {
					t.Errorf("expected env value redacted to %q, got %q", redactedValue, p.Env[0].Value)
				}
				if strings.Contains(p.Env[0].Value, "super-secret-value") {
					t.Errorf("secret env value leaked: %q", p.Env[0].Value)
				}
				// Empty value stays empty (nothing to redact).
				if p.Env[1].Value != "" {
					t.Errorf("expected empty env value to remain empty, got %q", p.Env[1].Value)
				}
			},
		},
		{
			name: "secrets redacted across multiple providers and env vars",
			config: &kubeletconfig.CredentialProviderConfig{
				Providers: []kubeletconfig.CredentialProvider{
					{
						Name:                 "first",
						MatchImages:          []string{"registry.io/foobar"},
						DefaultCacheDuration: &metav1.Duration{},
						APIVersion:           "credentialprovider.kubelet.k8s.io/v1",
						Args:                 []string{"--password=first-arg-secret"},
						Env: []kubeletconfig.ExecEnvVar{
							{Name: "A", Value: "first-secret-a"},
							{Name: "B", Value: "first-secret-b"},
						},
					},
					{
						Name:                 "second",
						MatchImages:          []string{"*.azurecr.io"},
						DefaultCacheDuration: &metav1.Duration{},
						APIVersion:           "credentialprovider.kubelet.k8s.io/v1",
						Args:                 []string{"--token=second-arg-secret"},
						Env: []kubeletconfig.ExecEnvVar{
							{Name: "C", Value: "second-secret-c"},
						},
					},
				},
			},
			validate: func(t *testing.T, cfg *configv1.CredentialProviderConfig) {
				if len(cfg.Providers) != 2 {
					t.Fatalf("expected 2 providers, got %d", len(cfg.Providers))
				}
				secrets := []string{
					"first-arg-secret", "second-arg-secret",
					"first-secret-a", "first-secret-b", "second-secret-c",
				}
				for _, p := range cfg.Providers {
					for _, a := range p.Args {
						if a != redactedValue {
							t.Errorf("provider %q: expected arg redacted, got %q", p.Name, a)
						}
					}
					for _, e := range p.Env {
						if e.Value != redactedValue {
							t.Errorf("provider %q: expected env %q value redacted, got %q", p.Name, e.Name, e.Value)
						}
					}
				}
				rendered := fmt.Sprintf("%+v", cfg)
				for _, s := range secrets {
					if strings.Contains(rendered, s) {
						t.Errorf("secret %q leaked in redacted config", s)
					}
				}
			},
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			setCredentialProviderConfig(nil)
			t.Cleanup(func() { setCredentialProviderConfig(nil) })

			if tc.config != nil {
				setCredentialProviderConfig(tc.config)
			}

			cfg, err := GetRedactedCredentialProviderConfig()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.validate == nil {
				if cfg != nil {
					t.Fatalf("expected nil config, got %+v", cfg)
				}
				return
			}
			if cfg == nil {
				t.Fatal("expected non-nil config")
			}
			tc.validate(t, cfg)
		})
	}
}

// TestGetRedactedCredentialProviderConfig_CachedConfigNotMutated verifies that the
// cached config is not mutated by redaction or by callers modifying the returned object.
func TestGetRedactedCredentialProviderConfig_CachedConfigNotMutated(t *testing.T) {
	t.Cleanup(func() { setCredentialProviderConfig(nil) })

	setCredentialProviderConfig(&kubeletconfig.CredentialProviderConfig{
		Providers: []kubeletconfig.CredentialProvider{{
			Name:                 "registered-provider",
			MatchImages:          []string{"registry.io/foobar"},
			DefaultCacheDuration: &metav1.Duration{},
			APIVersion:           "credentialprovider.kubelet.k8s.io/v1",
			Args:                 []string{"--token=real-secret"},
			Env: []kubeletconfig.ExecEnvVar{
				{Name: "SECRET_KEY", Value: "super-secret-value"},
			},
		}},
	})

	cfg, err := GetRedactedCredentialProviderConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "registered-provider" {
		t.Fatalf("expected cached config to be returned, got %+v", cfg.Providers)
	}
	// Redacted output must not contain secrets.
	if cfg.Providers[0].Env[0].Value != redactedValue {
		t.Errorf("expected env value redacted, got %q", cfg.Providers[0].Env[0].Value)
	}

	// The returned object must be a copy so callers cannot mutate the cached config.
	cfg.Providers[0].Name = "mutated"
	again, err := GetRedactedCredentialProviderConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if again.Providers[0].Name != "registered-provider" {
		t.Fatalf("cached config was mutated through returned value: %q", again.Providers[0].Name)
	}
	// Verify the cached internal config still has the original secret values
	// (redaction must not corrupt the cache via the unsafe-pointer alias).
	cached := getCredentialProviderConfig()
	if cached.Providers[0].Env[0].Value != "super-secret-value" {
		t.Fatalf("cached config was corrupted by redaction: env value = %q", cached.Providers[0].Env[0].Value)
	}
	if cached.Providers[0].Args[0] != "--token=real-secret" {
		t.Fatalf("cached config was corrupted by redaction: args = %v", cached.Providers[0].Args)
	}
}
