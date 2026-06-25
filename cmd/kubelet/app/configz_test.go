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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/component-base/configz"
)

func TestInitCredentialProviderConfigz(t *testing.T) {
	const configData = `---
kind: CredentialProviderConfig
apiVersion: kubelet.config.k8s.io/v1
providers:
  - name: test
    matchImages:
    - "registry.io/foobar"
    defaultCacheDuration: 10m
    apiVersion: credentialprovider.kubelet.k8s.io/v1
    env:
    - name: AWS_SECRET_ACCESS_KEY
      value: super-secret-value
`

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configData), 0600); err != nil {
		t.Fatal(err)
	}

	// configz uses a process-global registry, so clean up afterwards to avoid
	// leaking the registration into other tests.
	defer configz.Delete("credentialproviderconfig")

	if err := initCredentialProviderConfigz(context.Background(), configPath); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	mux := http.NewServeMux()
	configz.InstallHandler(mux)
	req := httptest.NewRequest(http.MethodGet, configz.DefaultConfigzPath, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "credentialproviderconfig") {
		t.Errorf("expected configz output to include credentialproviderconfig key, got: %s", body)
	}
	if !strings.Contains(body, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("expected env var name to be present, got: %s", body)
	}
	if strings.Contains(body, "super-secret-value") {
		t.Errorf("expected secret env value to be redacted, but it leaked into configz: %s", body)
	}
	// configz serializes via encoding/json, which HTML-escapes "<" and ">" in
	// the redaction placeholder ("<redacted>"), so match on the inner token.
	if !strings.Contains(body, "redacted") {
		t.Errorf("expected redacted placeholder in configz output, got: %s", body)
	}
}

func TestInitCredentialProviderConfigzEmptyPathIsNoop(t *testing.T) {
	defer configz.Delete("credentialproviderconfig")
	if err := initCredentialProviderConfigz(context.Background(), ""); err != nil {
		t.Fatalf("unexpected error for empty path: %v", err)
	}
	// With an empty path nothing should be registered, so a subsequent
	// registration under the same name must succeed.
	if _, err := configz.New("credentialproviderconfig"); err != nil {
		t.Fatalf("expected no prior registration for empty path, but New failed: %v", err)
	}
}
