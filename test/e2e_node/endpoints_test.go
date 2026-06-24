/*
Copyright The Kubernetes Authors.

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

package e2enode

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	credentialproviderv1 "k8s.io/kubelet/config/v1"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/pkg/cluster/ports"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/test/e2e/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// redactedEnvValue mirrors the placeholder substituted by the kubelet for
// non-empty credential provider env values before they are published via
// /configz (see cmd/kubelet/app/configz_credentialprovider.go).
const redactedEnvValue = "[REDACTED]"

// configzWrapper is a wrapper for the KubeletConfiguration returned by the /configz endpoint.
type configzWrapper struct {
	ComponentConfig kubeletconfigv1beta1.KubeletConfiguration `json:"kubeletconfig"`
}

// kubeletFlagsConfigz is a minimal, test-local view of the "kubeletflags"
// /configz entry (the command-line flags that are not merged into
// KubeletConfiguration). It mirrors the relevant fields of
// cmd/kubelet/app.KubeletFlagsConfigz; only the fields asserted on are included
// to avoid importing the kubelet command package into the e2e_node suite.
type kubeletFlagsConfigz struct {
	metav1.TypeMeta                   `json:",inline"`
	RootDirectory                     string `json:"rootDirectory,omitempty"`
	ImageCredentialProviderConfigPath string `json:"imageCredentialProviderConfig,omitempty"`
	ImageCredentialProviderBinDir     string `json:"imageCredentialProviderBinDir,omitempty"`
}

// fetchKubeletConfigz issues the authenticated HTTPS GET against the kubelet
// /configz endpoint and returns the raw response body.
func fetchKubeletConfigz(ctx context.Context) ([]byte, error) {
	endpoint := fmt.Sprintf("https://127.0.0.1:%d/configz", ports.KubeletPort)
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: tr}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", framework.TestContext.BearerToken))
	req.Header.Add("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			framework.Logf("Error closing response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/configz response status not 200, was: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// getKubeletConfigz fetches and decodes the kubelet's configuration from the /configz endpoint.
func getKubeletConfigz(ctx context.Context) (*configzWrapper, error) {
	respBody, err := fetchKubeletConfigz(ctx)
	if err != nil {
		return nil, err
	}

	configz := &configzWrapper{}
	err = json.Unmarshal(respBody, configz)
	if err != nil {
		return nil, err
	}
	return configz, nil
}

// getKubeletConfigzRaw fetches the /configz endpoint and returns the top-level
// JSON object keyed by registration name (e.g. "kubeletconfig",
// "kubeletflags", "credentialproviderconfig"). Entries that are not registered
// are simply absent from the map, which lets a test detect optional entries.
func getKubeletConfigzRaw(ctx context.Context) (map[string]json.RawMessage, error) {
	respBody, err := fetchKubeletConfigz(ctx)
	if err != nil {
		return nil, err
	}

	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(respBody, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// Serial because it has a test case for config reloading and kubelet restart.
var _ = SIGDescribe("Kubelet Endpoints", framework.WithNodeConformance(), framework.WithSerial(), func() {
	f := framework.NewDefaultFramework("kubelet-endpoints-test")
	f.NamespacePodSecurityLevel = admissionapi.LevelPrivileged

	ginkgo.It("should return APIVersion and Kind fields in /configz", func(ctx context.Context) {
		var configz *configzWrapper
		ginkgo.By("getting initial /configz")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			var err error
			configz, err = getKubeletConfigz(ctx)
			return err
		}, 1*time.Minute, 5*time.Second).Should(gomega.Succeed())

		gomega.Expect(configz.ComponentConfig.APIVersion).To(gomega.Equal("kubelet.config.k8s.io/v1beta1"))
		gomega.Expect(configz.ComponentConfig.Kind).To(gomega.Equal("KubeletConfiguration"))
	})

	ginkgo.It("should expose the non-merged command-line flags under kubeletflags in /configz", func(ctx context.Context) {
		var raw map[string]json.RawMessage
		ginkgo.By("getting /configz")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			var err error
			raw, err = getKubeletConfigzRaw(ctx)
			return err
		}, 1*time.Minute, 5*time.Second).Should(gomega.Succeed())

		gomega.Expect(raw).To(gomega.HaveKey("kubeletflags"))

		var flags kubeletFlagsConfigz
		framework.ExpectNoError(json.Unmarshal(raw["kubeletflags"], &flags))

		gomega.Expect(flags.APIVersion).To(gomega.Equal("kubelet.config.k8s.io/v1alpha1"))
		gomega.Expect(flags.Kind).To(gomega.Equal("KubeletFlags"))
		// The kubelet always runs with a root directory (--root-dir defaults to
		// /var/lib/kubelet), so this non-merged flag must round-trip.
		gomega.Expect(flags.RootDirectory).NotTo(gomega.BeEmpty())
	})

	ginkgo.It("should expose the credential provider config under credentialproviderconfig in /configz when configured", func(ctx context.Context) {
		var raw map[string]json.RawMessage
		ginkgo.By("getting /configz")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			var err error
			raw, err = getKubeletConfigzRaw(ctx)
			return err
		}, 1*time.Minute, 5*time.Second).Should(gomega.Succeed())

		if _, ok := raw["credentialproviderconfig"]; !ok {
			ginkgo.Skip("kubelet was not started with --image-credential-provider-config; credentialproviderconfig entry not present in /configz")
		}

		var cpc credentialproviderv1.CredentialProviderConfig
		framework.ExpectNoError(json.Unmarshal(raw["credentialproviderconfig"], &cpc))

		gomega.Expect(cpc.APIVersion).To(gomega.Equal("kubelet.config.k8s.io/v1"))
		gomega.Expect(cpc.Kind).To(gomega.Equal("CredentialProviderConfig"))
		gomega.Expect(cpc.Providers).NotTo(gomega.BeEmpty())

		for _, p := range cpc.Providers {
			gomega.Expect(p.Name).NotTo(gomega.BeEmpty())
			gomega.Expect(p.MatchImages).NotTo(gomega.BeEmpty())
			// Any static env value must be redacted; empty values stay empty.
			for _, e := range p.Env {
				if e.Value != "" {
					gomega.Expect(e.Value).To(gomega.Equal(redactedEnvValue), "credential provider env value for %q/%q must be redacted", p.Name, e.Name)
				}
			}
		}
	})

	ginkgo.Context("when config is updated", func() {
		var newFileCheckFrequency metav1.Duration
		tempSetCurrentKubeletConfig(f, func(ctx context.Context, initialConfig *kubeletconfig.KubeletConfiguration) {
			newFileCheckFrequency = metav1.Duration{Duration: 30 * time.Second}
			initialConfig.FileCheckFrequency = newFileCheckFrequency // this is just a sample field. Any other field would do here for a test.
		})

		ginkgo.It("should be reflected in /configz", func(ctx context.Context) {
			ginkgo.By("waiting for the updated /configz to be reflected")
			gomega.Eventually(ctx, func(ctx context.Context) error {
				resp, err := getKubeletConfigz(ctx)
				if err != nil {
					return err
				}
				updatedConfig := &resp.ComponentConfig
				if updatedConfig.FileCheckFrequency != newFileCheckFrequency {
					return fmt.Errorf("config not yet reflected, expected %v, got %v", newFileCheckFrequency, updatedConfig.FileCheckFrequency)
				}
				return nil
			}, 1*time.Minute, 5*time.Second).Should(gomega.Succeed())

			ginkgo.By("getting updated /configz")
			resp, err := getKubeletConfigz(ctx)
			framework.ExpectNoError(err)
			updatedConfig := &resp.ComponentConfig

			gomega.Expect(updatedConfig.APIVersion).To(gomega.Equal("kubelet.config.k8s.io/v1beta1"))
			gomega.Expect(updatedConfig.Kind).To(gomega.Equal("KubeletConfiguration"))
			gomega.Expect(updatedConfig.FileCheckFrequency).To(gomega.Equal(newFileCheckFrequency))
		})
	})
})
