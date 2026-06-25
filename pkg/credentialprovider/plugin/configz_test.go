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
	"os"
	"path/filepath"
	"strings"
	"testing"

	configv1 "k8s.io/kubelet/config/v1"
)

func TestGetCredentialProviderConfig(t *testing.T) {
	testcases := []struct {
		name       string
		configData string
		expectErr  string
		validate   func(t *testing.T, cfg *configv1.CredentialProviderConfig)
	}{
		{
			name: "env values are redacted but names preserved",
			configData: `---
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
  - name: test
    matchImages:
    - "registry.io/foobar"
    defaultCacheDuration: 10m
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    args:
    - --v=5
    env:
    - name: AWS_SECRET_ACCESS_KEY
      value: super-secret-value
    - name: EMPTY
      value: ""`,
			validate: func(t *testing.T, cfg *configv1.CredentialProviderConfig) {
				if cfg.Kind != "CredentialProviderConfig" {
					t.Errorf("expected Kind CredentialProviderConfig, got %q", cfg.Kind)
				}
				if cfg.APIVersion != configv1.SchemeGroupVersion.String() {
					t.Errorf("expected APIVersion %q, got %q", configv1.SchemeGroupVersion.String(), cfg.APIVersion)
				}
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
			configData: `---
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
  - name: first
    matchImages:
    - "registry.io/foobar"
    defaultCacheDuration: 10m
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    args:
    - --password=first-arg-secret
    env:
    - name: A
      value: first-secret-a
    - name: B
      value: first-secret-b
  - name: second
    matchImages:
    - "*.azurecr.io"
    defaultCacheDuration: 5m
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    args:
    - --token=second-arg-secret
    env:
    - name: C
      value: second-secret-c`,
			validate: func(t *testing.T, cfg *configv1.CredentialProviderConfig) {
				if len(cfg.Providers) != 2 {
					t.Fatalf("expected 2 providers, got %d", len(cfg.Providers))
				}
				// Every secret-bearing value across every provider must be redacted,
				// guarding against indexing bugs in the redaction loops.
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
				// Belt-and-suspenders: no secret may appear anywhere in the converted object.
				rendered := fmt.Sprintf("%+v", cfg)
				for _, s := range secrets {
					if strings.Contains(rendered, s) {
						t.Errorf("secret %q leaked in redacted config", s)
					}
				}
			},
		},
		{
			name:       "empty path returns error",
			configData: "",
			expectErr:  "credential provider config path is empty",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.name, func(t *testing.T) {
			var configPath string
			if tc.configData != "" {
				dir := t.TempDir()
				configPath = filepath.Join(dir, "config.yaml")
				if err := os.WriteFile(configPath, []byte(tc.configData), 0600); err != nil {
					t.Fatal(err)
				}
			}

			cfg, err := GetCredentialProviderConfig(configPath)
			if tc.expectErr != "" {
				if err == nil {
					t.Fatalf("expected error %q but got none", tc.expectErr)
				}
				if !strings.Contains(err.Error(), tc.expectErr) {
					t.Fatalf("expected error %q but got %q", tc.expectErr, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tc.validate(t, cfg)
		})
	}
}
