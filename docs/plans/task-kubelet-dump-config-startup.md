# Dump full effective config on kubelet startup instead of command line arguments

## Overview

Today, on startup the kubelet calls `cliflag.PrintFlags(cleanFlagSet)` which logs every
flag as `FLAG: --name="value"`. For flags that mirror `KubeletConfiguration` fields, the
logged value is the value parsed from the command line **before** the config file and
drop-in config directory are loaded/merged. After `--config` / `--config-dir` are applied
and flag precedence is re-enforced, the effective `KubeletConfiguration` can differ from
what was printed. The result is that the startup log shows **stale / misleading** values
that do not reflect the configuration the kubelet actually runs with.

This is a real, triage-accepted bug:
- Issue: kubernetes/kubernetes #122736 — "Printing CLI parameters for Kubelet is
  misleading to users." (e.g. `FLAG: --fail-swap-on="true"` logged while the code
  actually uses `false` from the KubeletConfig). Labels: `kind/bug`, `sig/node`,
  `triage/accepted`.
- Related PR exploring a fix: kubernetes/kubernetes #138216 — "kubelet: log FLAGS against
  effective KubeletConfiguration".

This plan changes the kubelet so that, instead of dumping the raw command-line flag
values, it dumps the **full effective `KubeletConfiguration`** at startup. The dump is
serialized the same way `/configz` serializes it: the internal config is converted to the
external `kubelet.config.k8s.io/v1beta1` type and marshaled. This config already reflects
all overrides — defaults, `--config` file, drop-in `--config-dir` files, and command-line
flag precedence — so the administrator sees exactly what the kubelet is running with.

Node-specific command-line settings that are **not** part of `KubeletConfiguration`
(`KubeletFlags`, e.g. `--hostname-override`, `--kubeconfig`, `--node-ip`, `--cert-dir`)
are accurate because they come straight from the command line, so the raw flag printing is
retained for those at a lower (more verbose) log level rather than removed entirely.

## Context

- Files involved:
  - Modify: `cmd/kubelet/app/server.go`
    - `NewKubeletCommand` / `RunE` closure around line 252, where
      `cliflag.PrintFlags(cleanFlagSet)` is currently called (right after config load,
      drop-in merge, `kubeletConfigFlagPrecedence`, and logging init).
    - Reuse / lightly refactor `setConfigz` (lines ~558-569) which already converts the
      internal `KubeletConfiguration` to `kubeletconfigv1beta1.KubeletConfiguration` via
      `kubeletscheme.NewSchemeAndCodecs()` + `scheme.Convert`.
    - Reuse the existing sensitive-field masking pattern at lines ~287-291
      (`StaticPodURLHeader` entries replaced with `<masked>`).
  - Modify: `cmd/kubelet/app/server_test.go`
    - Add unit tests next to the existing table-driven `TestMergeKubeletConfigurations`
      (line ~108) and `TestMergeKubeletConfigsWithSubdirs`.

- Related patterns:
  - `/configz` serialization: `setConfigz` (`cmd/kubelet/app/server.go`) converts internal
    → v1beta1 and `cz.Set(versioned)`; the configz package marshals the versioned object
    to JSON. See also `pkg/kubelet/server/server.go` (`configz.InstallHandler`) and
    `staging/src/k8s.io/component-base/configz/configz.go`.
  - Conversion helpers already imported in `server.go`:
    `kubeletscheme "k8s.io/kubernetes/pkg/kubelet/apis/config/scheme"`,
    `kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"`,
    `kubeletconfigv1beta1conversion "k8s.io/kubernetes/pkg/kubelet/apis/config/v1beta1"`.
    `mergeKubeletConfigurations` (line ~344) already shows the
    `Convert_config_KubeletConfiguration_To_v1beta1_KubeletConfiguration` path.
  - Sensitive masking: `StaticPodURLHeader` masked before the existing
    `logger.V(5).Info("KubeletConfiguration", ...)` dump (line ~301).
  - Flag printing helper: `cliflag.PrintFlags` →
    `staging/src/k8s.io/component-base/cli/flag/flags.go` (logs at `klog.V(1)`).

- Dependencies: none new. Uses existing in-tree packages
  (`kubeletscheme`, `kubeletconfigv1beta1`, `sigs.k8s.io/yaml` or `encoding/json`, `klog`).

## Development Approach

- **Testing approach**: Follow the repository's existing testing practices. `server.go`
  has table-driven unit tests in `server_test.go`; add focused unit tests there for the
  new serialization/masking helper. Run `go test`, `go vet`, and `gofmt` on the touched
  packages.
- Complete each task fully before moving to the next.
- **CRITICAL: all tests must pass before starting next task**
- Keep the change minimal and logging-only: do not alter flag parsing, precedence, or the
  `/configz` endpoint behavior.

## Implementation Steps

### Task 1: Add a helper that serializes the effective KubeletConfiguration like /configz

Extract a reusable conversion+marshal helper so both the new startup dump and (optionally)
`setConfigz` share one code path for producing the v1beta1 representation. The helper
takes the internal `*kubeletconfiginternal.KubeletConfiguration`, returns the versioned
`*kubeletconfigv1beta1.KubeletConfiguration` (and/or a marshaled string).

**Files:**
- Modify: `cmd/kubelet/app/server.go`

- [x] Add a function `convertToVersionedKubeletConfig(kc *kubeletconfiginternal.KubeletConfiguration) (*kubeletconfigv1beta1.KubeletConfiguration, error)` that builds the scheme via `kubeletscheme.NewSchemeAndCodecs()`, converts with `scheme.Convert`, and sets the GVK to `kubeletconfigv1beta1.SchemeGroupVersion.WithKind("KubeletConfiguration")` (same logic currently inlined in `setConfigz`).
- [x] Refactor `setConfigz` to call `convertToVersionedKubeletConfig` so there is a single conversion path (behavior unchanged: it still calls `cz.Set(versioned)`).
- [x] Add a function `marshalKubeletConfigForLog(kc *kubeletconfiginternal.KubeletConfiguration) (string, error)` that: (a) `DeepCopy`s the config, (b) masks sensitive fields the same way as the existing V(5) dump (`StaticPodURLHeader` values → `[]string{"<masked>"}`), (c) converts via `convertToVersionedKubeletConfig`, and (d) marshals to a human-readable string (prefer `sigs.k8s.io/yaml` for readability; `encoding/json` is already imported and is an acceptable fallback to exactly match `/configz`). Document in a comment that the output mirrors `/configz`.
- [x] Write/update tests: add `TestMarshalKubeletConfigForLog` (or similarly named) verifying that (1) a config with non-default values (e.g. `FailSwapOn`, an eviction threshold) round-trips into the marshaled output with the effective values, (2) `apiVersion: kubelet.config.k8s.io/v1beta1` / `kind: KubeletConfiguration` appear, and (3) `StaticPodURLHeader` values are masked as `<masked>`.

### Task 2: Dump the effective config at startup instead of stale flags

Replace the misleading `cliflag.PrintFlags(cleanFlagSet)` call at startup with a dump of
the effective merged `KubeletConfiguration`, while preserving the node-specific
`KubeletFlags` info at a more verbose level.

**Files:**
- Modify: `cmd/kubelet/app/server.go`

- [x] At the startup site (currently line ~252, after logging init and after the
  config/flag merge + precedence), call `marshalKubeletConfigForLog(kubeletConfig)` and log
  the result, e.g. `logger.Info("Effective KubeletConfiguration", "config", <yaml string>)`
  (use `klog`/`logger` already in scope). Log a clear, greppable header so operators can
  find it. On marshal error, log the error and fall back to the previous behavior so
  startup never fails because of logging.
- [x] Move the raw `cliflag.PrintFlags(cleanFlagSet)` call to a more verbose level so the
  node-specific `KubeletFlags` (hostname-override, kubeconfig, node-ip, cert-dir, etc.)
  remain available for debugging but no longer present misleading config-flag values as the
  primary startup output. (PrintFlags logs at V(1); keep that, but the effective-config dump
  is the authoritative, always-visible startup line. Add a short comment referencing issue
  #122736 explaining why flags alone are misleading.)
- [x] Confirm the existing `logger.V(5).Info("KubeletConfiguration", ...)` masked dump at
  line ~301 is now redundant with the new dump; either remove it or keep it — if kept,
  ensure the two do not duplicate confusingly (prefer reusing `marshalKubeletConfigForLog`
  there too). Document the decision in a comment. (Removed the redundant V(5) dump and its
  masking block; documented the removal in a comment.)
- [x] Write/update tests: add a test that drives the merge path (reuse the
  `TestMergeKubeletConfigurations` fixtures: a base `--config` value overridden by a
  drop-in `.conf` file) and asserts that `marshalKubeletConfigForLog` on the resulting
  merged config reflects the **post-merge** effective value, not the pre-merge default —
  directly demonstrating the #122736 fix.

### Task 3: Validate, document the issue reference, and finalize

**Files:**
- Modify: `cmd/kubelet/app/server.go`, `cmd/kubelet/app/server_test.go`

- [x] Run `gofmt -w` on the changed files and `go vet ./cmd/kubelet/...`. (gofmt -l reports no issues; go vet clean.)
- [x] Run `go test ./cmd/kubelet/app/...` and ensure all tests pass. (All pass, incl. TestMarshalKubeletConfigForLog and TestMergeKubeletConfigurations.)
- [x] Run `go build ./cmd/kubelet/...` to confirm the kubelet still builds. (Builds successfully.)
- [x] Ensure a code comment near the new dump references issue #122736 (misleading CLI
  parameter logging) so future readers understand the rationale. (Referenced at server.go:260 and in marshalKubeletConfigForLog doc comment.)
- [x] Confirm no change to `/configz` HTTP behavior, flag parsing, or precedence — the
  change is logging-only. (setConfigz still calls cz.Set(versioned); PrintFlags still called; precedence untouched.)
