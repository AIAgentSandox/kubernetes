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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-base/configz"
	kubeletconfiginternal "k8s.io/kubernetes/pkg/kubelet/apis/config"
)

const testCredentialProviderConfig = `---
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1alpha1
providers:
  - name: test
    matchImages:
    - "registry.io/foobar"
    defaultCacheDuration: 10m
    apiVersion: credentialprovider.kubelet.k8s.io/v1alpha1
    args:
    - --v=5
    env:
    - name: FOO
      value: BAR
    - name: EMPTY
      value: ""
`

func TestRedactCredentialProviderConfig(t *testing.T) {
	cfg := &kubeletconfiginternal.CredentialProviderConfig{
		Providers: []kubeletconfiginternal.CredentialProvider{
			{
				Name:                 "test",
				MatchImages:          []string{"registry.io/foobar"},
				DefaultCacheDuration: &metav1.Duration{Duration: 10 * time.Minute},
				APIVersion:           "credentialprovider.kubelet.k8s.io/v1alpha1",
				Args:                 []string{"--v=5"},
				Env: []kubeletconfiginternal.ExecEnvVar{
					{Name: "FOO", Value: "secret-token"},
					{Name: "EMPTY", Value: ""},
				},
			},
		},
	}

	redacted := redactCredentialProviderConfig(cfg)

	// Non-empty env values are redacted; empty ones are left untouched.
	require.Equal(t, "FOO", redacted.Providers[0].Env[0].Name)
	require.Equal(t, redactedEnvValue, redacted.Providers[0].Env[0].Value)
	require.Equal(t, "EMPTY", redacted.Providers[0].Env[1].Name)
	require.Equal(t, "", redacted.Providers[0].Env[1].Value)

	// All other fields are preserved.
	require.Equal(t, "test", redacted.Providers[0].Name)
	require.Equal(t, []string{"registry.io/foobar"}, redacted.Providers[0].MatchImages)
	require.Equal(t, []string{"--v=5"}, redacted.Providers[0].Args)
	require.Equal(t, "credentialprovider.kubelet.k8s.io/v1alpha1", redacted.Providers[0].APIVersion)

	// The original config must not be mutated.
	require.Equal(t, "secret-token", cfg.Providers[0].Env[0].Value)
}

// TestInitCredentialProviderConfigz exercises the full configz registration path
// and asserts the JSON served by the /configz handler is keyed
// "credentialproviderconfig", carries the expected apiVersion/kind, contains the
// configured provider, and redacts non-empty env values.
func TestInitCredentialProviderConfigz(t *testing.T) {
	const name = "credentialproviderconfig"
	configz.Delete(name)
	t.Cleanup(func() { configz.Delete(name) })

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(testCredentialProviderConfig), 0600))

	require.NoError(t, initCredentialProviderConfigz(context.Background(), configPath))

	mux := http.NewServeMux()
	configz.InstallHandler(mux)
	req := httptest.NewRequest(http.MethodGet, configz.DefaultConfigzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))

	raw, ok := payload[name]
	require.True(t, ok, "expected /configz JSON to contain key %q", name)

	var entry struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Providers  []struct {
			Name        string `json:"name"`
			MatchImages []string
			Env         []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"env"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(raw, &entry))

	require.Equal(t, "kubelet.config.k8s.io/v1", entry.APIVersion)
	require.Equal(t, "CredentialProviderConfig", entry.Kind)
	require.Len(t, entry.Providers, 1)
	require.Equal(t, "test", entry.Providers[0].Name)
	require.Equal(t, []string{"registry.io/foobar"}, entry.Providers[0].MatchImages)

	require.Len(t, entry.Providers[0].Env, 2)
	require.Equal(t, "FOO", entry.Providers[0].Env[0].Name)
	require.Equal(t, redactedEnvValue, entry.Providers[0].Env[0].Value)
	require.Equal(t, "EMPTY", entry.Providers[0].Env[1].Name)
	require.Equal(t, "", entry.Providers[0].Env[1].Value)
}

// TestInitCredentialProviderConfigzLoadFailure asserts that a load/parse failure
// is tolerated: initCredentialProviderConfigz returns nil (kubelet startup is not
// blocked) and no "credentialproviderconfig" entry is registered.
func TestInitCredentialProviderConfigzLoadFailure(t *testing.T) {
	const name = "credentialproviderconfig"
	configz.Delete(name)
	t.Cleanup(func() { configz.Delete(name) })

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("this: is: not: valid: yaml"), 0600))

	require.NoError(t, initCredentialProviderConfigz(context.Background(), configPath))

	mux := http.NewServeMux()
	configz.InstallHandler(mux)
	req := httptest.NewRequest(http.MethodGet, configz.DefaultConfigzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	_, ok := payload[name]
	require.False(t, ok, "expected /configz JSON to omit key %q when config fails to load", name)
}

// TestInitCredentialProviderConfigzEmptyPath asserts no entry is registered when
// the credential provider config path is empty.
func TestInitCredentialProviderConfigzEmptyPath(t *testing.T) {
	const name = "credentialproviderconfig"
	configz.Delete(name)
	t.Cleanup(func() { configz.Delete(name) })

	require.NoError(t, initCredentialProviderConfigz(context.Background(), ""))

	mux := http.NewServeMux()
	configz.InstallHandler(mux)
	req := httptest.NewRequest(http.MethodGet, configz.DefaultConfigzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var payload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	_, ok := payload[name]
	require.False(t, ok, "expected /configz JSON to omit key %q when path is empty", name)
}
