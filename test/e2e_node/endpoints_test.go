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
	configv1 "k8s.io/kubelet/config/v1"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/pkg/cluster/ports"
	"k8s.io/kubernetes/pkg/features"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/test/e2e/framework"
	admissionapi "k8s.io/pod-security-admission/api"
)

// configzWrapper is a wrapper for the KubeletConfiguration returned by the /configz endpoint.
type configzWrapper struct {
	ComponentConfig kubeletconfigv1beta1.KubeletConfiguration `json:"kubeletconfig"`
}

// credentialProviderConfigzWrapper is a wrapper for the CredentialProviderConfig
// returned by the /configz endpoint.
type credentialProviderConfigzWrapper struct {
	CredentialProviderConfig configv1.CredentialProviderConfig `json:"credentialproviderconfig"`
}

// fetchConfigz fetches the raw body of the kubelet's /configz endpoint.
func fetchConfigz(ctx context.Context) ([]byte, error) {
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
	respBody, err := fetchConfigz(ctx)
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

// getCredentialProviderConfigz fetches and decodes the kubelet's credential
// provider configuration from the /configz endpoint.
func getCredentialProviderConfigz(ctx context.Context) (*credentialProviderConfigzWrapper, error) {
	respBody, err := fetchConfigz(ctx)
	if err != nil {
		return nil, err
	}

	configz := &credentialProviderConfigzWrapper{}
	err = json.Unmarshal(respBody, configz)
	if err != nil {
		return nil, err
	}
	return configz, nil
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

	// The credential provider config is only registered with /configz when the
	// kubelet is started with --image-credential-provider-config. The node e2e
	// remote runner configures the gcp-credential-provider plugin together with
	// the KubeletServiceAccountTokenForCredentialProviders feature gate, so this
	// test is gated on that feature gate to ensure it only runs when a credential
	// provider config is present.
	framework.It("should return the credential provider config in /configz", framework.WithFeatureGate(features.KubeletServiceAccountTokenForCredentialProviders), func(ctx context.Context) {
		var configz *credentialProviderConfigzWrapper
		ginkgo.By("getting the credential provider config from /configz")
		gomega.Eventually(ctx, func(ctx context.Context) error {
			var err error
			configz, err = getCredentialProviderConfigz(ctx)
			if err != nil {
				return err
			}
			if len(configz.CredentialProviderConfig.Providers) == 0 {
				return fmt.Errorf("credentialproviderconfig not yet present in /configz")
			}
			return nil
		}, 1*time.Minute, 5*time.Second).Should(gomega.Succeed())

		cpConfig := configz.CredentialProviderConfig
		gomega.Expect(cpConfig.APIVersion).To(gomega.Equal("kubelet.config.k8s.io/v1"))
		gomega.Expect(cpConfig.Kind).To(gomega.Equal("CredentialProviderConfig"))

		ginkgo.By("verifying the configured credential provider is reflected in /configz")
		provider, found := findProvider(cpConfig.Providers, "gcp-credential-provider")
		gomega.Expect(found).To(gomega.BeTrue(), "expected gcp-credential-provider in /configz, got %+v", cpConfig.Providers)
		gomega.Expect(provider.MatchImages).To(gomega.ContainElement("gcr.io"))

		ginkgo.By("verifying sensitive credential provider env values are redacted")
		// The gcp-credential-provider-with-sa provider is configured with a
		// non-empty env value (PLUGIN_MODE) which must be redacted when exposed.
		saProvider, found := findProvider(cpConfig.Providers, "gcp-credential-provider-with-sa")
		gomega.Expect(found).To(gomega.BeTrue(), "expected gcp-credential-provider-with-sa in /configz, got %+v", cpConfig.Providers)
		gomega.Expect(saProvider.Env).NotTo(gomega.BeEmpty())
		for _, env := range saProvider.Env {
			gomega.Expect(env.Value).To(gomega.Equal("<redacted>"), "expected env %q value to be redacted", env.Name)
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

// findProvider returns the credential provider with the given name, and whether it was found.
func findProvider(providers []configv1.CredentialProvider, name string) (configv1.CredentialProvider, bool) {
	for _, p := range providers {
		if p.Name == name {
			return p, true
		}
	}
	return configv1.CredentialProvider{}, false
}
