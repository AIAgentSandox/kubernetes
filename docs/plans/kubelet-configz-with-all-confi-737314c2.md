# Kubelet /configz with all configuration sources

## Overview

Today the kubelet only exposes its primary `KubeletConfiguration` object via the
`/configz` HTTP endpoint (registered under the key `kubeletconfig`). The kubelet,
however, is configured from several additional sources that are **not** part of
`KubeletConfiguration`:

1. **Command-line flags that are never merged into `KubeletConfiguration`** — the
   fields of the `KubeletFlags` struct (kubeconfig path, bootstrap kubeconfig
   path, cert dir, root dir, container-runtime options including the image
   credential provider config/bin dir, node labels, lock file, deprecated GC
   flags, etc.). Flags that *are* merged into `KubeletConfiguration` (e.g.
   `--pod-manifest-path`, `--tls-cert-file`, `--client-ca-file`) are already
   visible in the existing `kubeletconfig` entry, so they are out of scope.
2. **The image credential provider config file** (`--image-credential-provider-config`),
   which is a separate `CredentialProviderConfig` object loaded outside of
   `KubeletConfiguration`.

Drop-in config files (`--config-dir`) are explicitly out of scope because they
are already merged into the primary `KubeletConfiguration` before it is published
to `/configz`.

This plan adds two new named entries to `/configz` (`kubeletflags` and
`credentialproviderconfig`), performs a secrets analysis of every additional
source, redacts the one field that can legitimately hold a static secret
(credential-provider `env` values), and adds e2e_node test coverage.

The `component-base/configz` package already supports multiple named entries:
`configz.New(name)` registers an additional ComponentConfig and the handler
returns a JSON map keyed by registration name, e.g.
`{"kubeletconfig": {...}, "kubeletflags": {...}, "credentialproviderconfig": {...}}`.
`configz.Set` only requires the value to be a `runtime.Object` carrying an
external GroupVersionKind (it does not validate against a registered scheme).

## Context

- Files involved:
  - `cmd/kubelet/app/server.go` — `initConfigz`, `setConfigz` (lines ~558-583);
    `/configz` is initialized from `run()` at `initConfigz(ctx, &s.KubeletConfiguration)`.
    The full `*options.KubeletServer` (which embeds `KubeletFlags`) is available
    in `run()`, so the flags can be passed to a new init function.
  - `cmd/kubelet/app/options/options.go` — `KubeletFlags` struct (lines ~54-134),
    the source of the non-merged command-line flags. Embeds
    `kubeletconfig.ContainerRuntimeOptions`.
  - `cmd/kubelet/app/server_test.go` — existing unit tests for the server package
    (location for `setConfigz`-style unit tests).
  - `pkg/kubelet/server/server.go` — `configz.InstallHandler(s.restfulCont)`
    (line ~649). No change needed; the handler already serves all registered
    entries.
  - `pkg/credentialprovider/plugin/config.go` — `readCredentialProviderConfig`
    (lines 50-121, currently **unexported**) decodes the credential provider
    config file/dir into the internal `kubeletconfig.CredentialProviderConfig`
    (`k8s.io/kubelet/pkg/apis/credentialprovider`). It also returns a sha256
    hash. `RegisterCredentialProviderPlugins(configPath, binDir, ...)` is the
    exported entry point already called during kubelet startup.
  - `pkg/credentialprovider/plugin/config_test.go` — existing unit tests for the
    config loader.
  - `staging/src/k8s.io/component-base/configz/configz.go` — `New`, `Set`,
    `InstallHandler`, `MarshalJSON` (no change expected).
  - `test/e2e_node/endpoints_test.go` — existing `/configz` e2e_node test
    ("Kubelet Endpoints"); contains `getKubeletConfigz` helper and
    `configzWrapper`. New assertions/wrapper fields are added here.
  - `test/e2e_node/util_kubeletconfig.go` / `test/e2e_node/util.go` — helpers for
    restarting the kubelet with custom flags/config used by the new e2e test.

- Related patterns:
  - `initConfigz`/`setConfigz` in `cmd/kubelet/app/server.go` — the canonical way
    a config object is converted to its external versioned type, stamped with a
    GVK, and registered via `configz.New` + `cz.Set`.
  - The external/versioned conversion pattern (`scheme.Convert(internal, versioned, nil)`
    then `SetGroupVersionKind(...)`) used for `KubeletConfiguration` should be
    reused for `CredentialProviderConfig` (it has registered internal +
    `v1`/`v1beta1`/`v1alpha1` external types and a scheme/codecs in
    `pkg/credentialprovider/plugin`).
  - The e2e_node `/configz` test pattern in `test/e2e_node/endpoints_test.go`
    (`gomega.Eventually` polling `getKubeletConfigz`, asserting fields).

- Dependencies: none new. All work uses existing Kubernetes libraries
  (`component-base/configz`, `apimachinery/runtime`, the kubelet config and
  credential-provider schemes, Ginkgo/Gomega for e2e_node tests).

## Secrets analysis (informs the design; finalized in Task 4)

| Source | Field(s) | Holds secret content? | Decision |
|---|---|---|---|
| `KubeletFlags` (all fields) | `KubeConfig`, `BootstrapKubeconfig`, `CertDirectory`, `CloudConfigFile`, `RootDirectory`, `ImageCredentialProviderConfigPath`, `ImageCredentialProviderBinDir`, `NodeLabels`, `LockFilePath`, deprecated GC flags, etc. | No — these are filesystem **paths**, identifiers, booleans, and labels, never the *contents* of secret files. `KubeletConfiguration` already exposes comparable paths (`TLSPrivateKeyFile`, `Authentication.X509.ClientCAFile`, `StaticPodPath`) via `/configz`. | Expose all fields as-is. |
| `CredentialProviderConfig` | `providers[].name`, `matchImages`, `defaultCacheDuration`, `apiVersion`, `args`, `tokenAttributes` | No — describe which plugin runs for which images and how tokens are requested. | Expose as-is. |
| `CredentialProviderConfig` | `providers[].env[].value` | **Potentially** — `env` injects static environment variables into the plugin process; an admin *could* place credentials there (discouraged but possible). | Redact: replace each non-empty `value` with `[REDACTED]`, keep `name`. |

The `/configz` endpoint is already RBAC-protected (`nodes/configz` subresource,
see `pkg/kubelet/server/auth.go`), but redacting `env[].value` is a defensive
measure consistent with treating that field as admin-controlled secret material.

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices —
  table-driven Go unit tests next to the code (`cmd/kubelet/app/server_test.go`,
  `pkg/credentialprovider/plugin/config_test.go`) and Ginkgo/Gomega e2e_node
  tests in `test/e2e_node`.
- Complete each task fully before moving to the next.
- **CRITICAL: all tests must pass before starting the next task.**
- Validation commands:
  - Build: `go build ./cmd/kubelet/... ./pkg/credentialprovider/...`
  - Unit tests: `go test ./cmd/kubelet/app/... ./pkg/credentialprovider/plugin/...`
  - e2e_node compile check: `go vet ./test/e2e_node/...`
  - e2e_node tests run: `make test-e2e-node TEST_ARGS='--kubelet-flags=--fail-swap-on=false' FOCUS="<related config tests>"`
  - Lint/format: `gofmt -l` on changed files (and `hack/verify-gofmt.sh` if run
    locally); run `goimports` on changed files.

## Implementation Steps

### Task 1: Expose non-merged command-line flags (`kubeletflags`) in `/configz`

Add a new `/configz` entry named `kubeletflags` that publishes the fields of
`KubeletFlags` (the command-line arguments that are never merged into
`KubeletConfiguration`). Because `configz.Set` requires a `runtime.Object` with
an external GVK, introduce a small exposable wrapper type.

**Files:**
- Create: `cmd/kubelet/app/configz_flags.go` (new exposable type + builder)
- Modify: `cmd/kubelet/app/server.go` (register the new entry from `run()`)
- Modify/Create: `cmd/kubelet/app/server_test.go` (unit tests)

- [x] Define an exposable, JSON-serializable type for the flags, e.g.
      `KubeletFlagsConfigz` embedding `metav1.TypeMeta` plus the relevant
      `KubeletFlags` fields with `json` tags (kubeconfig, bootstrap kubeconfig,
      hostname override, node IP, cert dir, cloud provider, cloud config file,
      root dir, kubelet config file, kubelet drop-in config directory,
      experimental mounter path, node labels, lock file path,
      exit-on-lock-contention, deprecated GC flags, and the embedded
      `ContainerRuntimeOptions` fields such as `ImageCredentialProviderConfigPath`
      and `ImageCredentialProviderBinDir`). Document why each field is included
      and that none carry secret contents (paths/IDs only).
      (Done: `KubeletFlagsConfigz` in `cmd/kubelet/app/configz_flags.go`.)
- [x] Make the mechanism of detecting whether the flag merged to the config or not universal
      (Done: `nonMergedFlagNames()` diffs the `KubeletFlags.AddFlags` flag set
      against the `AddKubeletConfigFlags` flag set rather than hardcoding;
      `kubeletFlagsConfigzCoveredFlags` is guarded against it by
      `TestKubeletFlagsConfigzCoversAllNonMergedFlags`.)
- [x] Implement `runtime.Object` on the type (`GetObjectKind()` via the embedded
      `TypeMeta`, plus a `DeepCopyObject()` method). Stamp a descriptive external
      GVK (e.g. group `kubelet.config.k8s.io`, an appropriate version, kind
      `KubeletFlags`) so `configz.Set` accepts it.
      (Done: GVK `kubelet.config.k8s.io/v1alpha1`, kind `KubeletFlags`.)
- [x] Add a builder function that maps an `*options.KubeletFlags` (or
      `*options.KubeletServer`) into the new type.
      (Done: `newKubeletFlagsConfigz(*options.KubeletFlags)`.)
- [x] In `cmd/kubelet/app/server.go`, register the entry: either extend
      `initConfigz` to also take the flags and call `configz.New("kubeletflags")`
      + `cz.Set(...)`, or add a sibling `initFlagsConfigz`. Wire it from `run()`
      where `initConfigz` is currently invoked (the `*options.KubeletServer`
      `s` is in scope). Log and return the error on failure, matching the
      existing `initConfigz` error handling.
      (Done: `initFlagsConfigz`, wired from `run()` next to `initConfigz`.)
- [x] Write/update unit tests in `cmd/kubelet/app/server_test.go`: build the
      flags type from a populated `KubeletFlags`, register/marshal it through the
      configz path, and assert (a) the JSON is keyed `kubeletflags`, (b) it
      carries the expected `apiVersion`/`kind`, and (c) representative fields
      (e.g. `imageCredentialProviderConfig`, `rootDirectory`) round-trip.
      (Done: `cmd/kubelet/app/configz_flags_test.go`.)
- [x] Validate: `go build ./cmd/kubelet/...` and
      `go test ./cmd/kubelet/app/...` pass; `gofmt`/`goimports` clean.

### Task 2: Expose the credential provider config (`credentialproviderconfig`) in `/configz`

Add a new `/configz` entry named `credentialproviderconfig` that publishes the
loaded `CredentialProviderConfig`, with `env[].value` redacted. Only register the
entry when `--image-credential-provider-config` is set and the file/dir loads
successfully.

**Files:**
- Modify: `pkg/credentialprovider/plugin/config.go` (export a reader returning the
  decoded config so `cmd/kubelet` can publish it without re-implementing parsing)
- Modify: `cmd/kubelet/app/server.go` (load, redact, version-convert, register)
- Modify/Create: `cmd/kubelet/app/server_test.go` and/or
  `pkg/credentialprovider/plugin/config_test.go` (unit tests, incl. redaction)

- [x] Expose the credential provider config to `cmd/kubelet`: add an exported
      wrapper in `pkg/credentialprovider/plugin/config.go` (e.g.
      `GetCredentialProviderConfig(path) (*kubeletconfig.CredentialProviderConfig, error)`)
      that calls the existing `readCredentialProviderConfig`, or export the reader
      directly. Keep the existing internal callers working.
      (Done: `GetCredentialProviderConfig` wraps `readCredentialProviderConfig`,
      dropping the hash; internal callers unchanged.)
- [x] In `cmd/kubelet/app/server.go`, after config load (where
      `ImageCredentialProviderConfigPath` is known), when the path is non-empty:
      load the config, deep-copy it, and **redact** every non-empty
      `providers[].env[].value` to `[REDACTED]`.
      (Done: `redactCredentialProviderConfig` in
      `cmd/kubelet/app/configz_credentialprovider.go` deep-copies via `DeepCopy()`
      and redacts non-empty env values; called from `initCredentialProviderConfigz`.)
- [x] Convert the (redacted) internal `CredentialProviderConfig` to its external
      versioned type using the credential-provider scheme/codecs, stamp the GVK,
      and register via `configz.New("credentialproviderconfig")` + `cz.Set(...)`
      (mirroring `setConfigz`).
      (Done: `setCredentialProviderConfigz` converts internal ->
      `k8s.io/kubelet/config/v1.CredentialProviderConfig` via
      `kubeletscheme.NewSchemeAndCodecs`, stamps GVK
      `kubelet.config.k8s.io/v1` kind `CredentialProviderConfig`.)
- [x] Handle the disabled/empty case gracefully: when the path is empty, do not
      register the entry (so `/configz` simply omits `credentialproviderconfig`).
      A load/parse failure should log and not crash kubelet startup beyond
      existing behavior — match how the credential provider is otherwise
      initialized.
      (Done: `initCredentialProviderConfigz` returns nil without registering when
      path is empty; a load error is logged and returns nil. Wired from `run()`
      next to `initConfigz`/`initFlagsConfigz`, logging-only on error.)
- [x] Write/update unit tests: (a) a redaction test asserting non-empty
      `env[].value` becomes `[REDACTED]` while `name`, `matchImages`, `args`,
      `tokenAttributes` are preserved; (b) a test that the registered entry is
      keyed `credentialproviderconfig` with the expected `apiVersion`/`kind`;
      (c) a test that no entry is registered when the path is empty.
      (Done: `cmd/kubelet/app/configz_credentialprovider_test.go`
      (`TestRedactCredentialProviderConfig`, `TestInitCredentialProviderConfigz`,
      `TestInitCredentialProviderConfigzEmptyPath`) plus
      `TestGetCredentialProviderConfig` in `config_test.go`.)
- [x] Validate: `go build ./cmd/kubelet/... ./pkg/credentialprovider/...` and
      `go test ./cmd/kubelet/app/... ./pkg/credentialprovider/plugin/...` pass;
      `gofmt`/`goimports` clean.

### Task 3: e2e_node tests for the new `/configz` entries

Extend the existing kubelet `/configz` e2e_node coverage to validate the new
entries end-to-end on a real kubelet.

**Files:**
- Modify: `test/e2e_node/endpoints_test.go`

- [ ] Extend `getKubeletConfigz` / the `configzWrapper` struct (or add a parallel
      raw-map fetch) so the test can read the `kubeletflags` and
      `credentialproviderconfig` keys in addition to `kubeletconfig`. Reuse the
      existing HTTPS + bearer-token request helper.
- [ ] Add a test case asserting the `kubeletflags` entry is present, carries the
      expected `apiVersion`/`kind`, and reflects a known flag value the node is
      started with (e.g. `imageCredentialProviderConfig`/`rootDirectory`, or a
      flag toggled via the e2e_node kubelet restart helpers in
      `util_kubeletconfig.go`/`util.go`).
- [ ] Add a test case for `credentialproviderconfig`: when the node's kubelet is
      configured with `--image-credential-provider-config`, assert the entry is
      present, has the expected `apiVersion`/`kind`, contains the configured
      provider name(s)/matchImages, and that any `env[].value` is redacted
      (`[REDACTED]`). If the standard e2e_node kubelet is not started with a
      credential provider config, gate this case appropriately (skip with a clear
      message, or use the restart helper to start the kubelet with a temporary
      credential provider config and revert afterward).
- [ ] Keep the suite `WithSerial` where kubelet restarts are involved, matching
      the existing "Kubelet Endpoints" suite conventions.
- [ ] Validate: `go vet ./test/e2e_node/...` compiles; `gofmt`/`goimports` clean.
      (Note: the e2e_node suite requires a running node environment to execute;
      compilation/vet is the automatable gate here.)