# credentialproviderconfig in configz

## Implementation Steps

### Task 1: credentialproviderconfig in configz

- [x] Add credentialproviderconfig to configz of kubelet. Decide if any of the fields must be hidden on serialization due to security concenrs.

  Implemented:
  - `pkg/credentialprovider/plugin/config.go`: added exported `GetCredentialProviderConfig(configPath)` that reads the credential provider config (file or directory), converts it to the `kubelet.config.k8s.io/v1` versioned type, sets the GVK, and redacts sensitive fields before returning.
  - `cmd/kubelet/app/server.go`: added `initCredentialProviderConfigz`, called from `run()` right after `initConfigz`, which registers the config under the `credentialproviderconfig` key on the existing `/configz` endpoint. It is a no-op when `--image-credential-provider-config` is unset.

  Security decision (fields hidden on serialization):
  - Redact each provider's `env[*].value` (replaced with `<redacted>`), because environment variable values commonly carry secrets (registry credentials, API keys). This mirrors the existing kubelet precedent of masking `StaticPodURLHeader` values in logs.
  - Env variable `name` is preserved so operators can see which variables are configured.
  - All other fields (`name`, `matchImages`, `args`, `defaultCacheDuration`, `apiVersion`, `tokenAttributes`) describe how the kubelet invokes the plugin rather than credential material and are surfaced as-is.

  Tests: `pkg/credentialprovider/plugin/configz_test.go` (redaction + empty-path error) and `cmd/kubelet/app/configz_test.go` (configz registration, secret non-leakage, empty-path no-op).
