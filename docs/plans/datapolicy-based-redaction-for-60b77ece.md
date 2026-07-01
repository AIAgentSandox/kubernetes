# Datapolicy-based Redaction for configz and Logging

## Implementation Steps

### Task 1: Datapolicy-based Redaction for configz and Logging

- [x] # Datapolicy-based Redaction for configz and Logging

## Overview
- Fix kubernetes/kubernetes#140101: kubelet `/configz` leaks `StaticPodURLHeader` credentials in cleartext
- Add a generic `Redact()` function to `datapol` package that replaces `datapolicy`-tagged field values with `"CLASSIFIED"`
- Wire it into configz serialization and kubelet logging, replacing hardcoded field-name masking
- Aligns with the flagz `CLASSIFIED` convention for zpages

## Context (from discovery)
- `datapolicy` struct tags already exist on ~58 fields across the codebase (token, password, security-key, secret-key)
- `StaticPodURLHeader` is already tagged `datapolicy:"token"` on both internal and v1beta1 types
- `datapol.Verify()` in component-base walks structs via reflection to find tagged fields but is never called in production
- `configz.MarshalJSON()` does plain `json.Marshal` with zero awareness of datapolicy tags
- `marshalKubeletConfigForLog()` hardcodes masking of `StaticPodURLHeader` only
- `RedactSecrets()` in client-go reads datapolicy tags for kubeconfig — similar pattern but uses "REDACTED"

## Development Approach
- **testing approach**: Regular (code first, then tests)
- complete each task fully before moving to the next
- make small, focused changes
- every task includes tests for code changes
- all tests must pass before starting next task
- run tests after each change

## Testing Strategy
- **unit tests**: test `Redact()` for all field types (string, []byte, []string, map[string][]string, nested structs)
- **unit tests**: test configz serialization redacts datapolicy-tagged fields
- **unit tests**: test kubelet logging uses Redact instead of hardcoded masking
- **integration**: the existing POC from the issue can verify the fix end-to-end

## Progress Tracking
- mark completed items with `[x]` immediately when done
- add newly discovered tasks with + prefix
- document issues/blockers with warning prefix
- update plan if implementation deviates from original scope

## Solution Overview
- Add `Redact(obj interface{})` to `staging/src/k8s.io/component-base/logs/datapol/datapol.go`
- Mutates in place — callers must pass a deep copy
- Walks struct fields via reflection; any field with a `datapolicy` tag gets its value replaced with `"CLASSIFIED"`
- `configz.MarshalJSON()` calls `val.DeepCopyObject()` then `datapol.Redact()` before `json.Marshal`
- `marshalKubeletConfigForLog()` replaces hardcoded `StaticPodURLHeader` loop with `datapol.Redact()` call

## Technical Details

**Redaction by type:**
| Field type | Redacted value |
|---|---|
| `string` | `"CLASSIFIED"` |
| `[]byte` | `[]byte("CLASSIFIED")` |
| `[]string` | `[]string{"CLASSIFIED"}` |
| `map[K]V` (tagged field) | preserve keys, replace leaf values with `"CLASSIFIED"` |

**Processing flow:**
1. Caller deep-copies the object
2. `datapol.Redact()` walks the copy via reflection
3. For each struct field with `datapolicy` tag (any value), replaces the value
4. For nested structs without the tag, recurses into children
5. Caller serializes the redacted copy

## Implementation Steps

### Task 1: Add Redact() function to datapol package

**Files:**
- Modify: `staging/src/k8s.io/component-base/logs/datapol/datapol.go`

- [x] add `Redact(obj interface{})` function that accepts any value and mutates in place
- [x] implement reflection walker: for struct fields with `datapolicy` tag, call `redactValue()`
- [x] implement `redactValue()` handling: string, []byte, slice of strings, map (preserve keys, redact values)
- [x] recurse into nested structs, slices, maps for untagged fields
- [x] handle pointer fields (dereference and recurse)

### Task 2: Add tests for Redact()

**Files:**
- Modify: `staging/src/k8s.io/component-base/logs/datapol/datapol_test.go`

- [x] test redaction of string field with datapolicy tag
- [x] test redaction of map[string][]string field (StaticPodURLHeader shape)
- [x] test that untagged fields are NOT redacted
- [x] test nested struct with tagged fields
- [x] test empty/zero-value fields (should still be redacted if tagged — the field exists, just empty)
- [x] test struct with no datapolicy tags (no-op)
- [x] run tests: `go test ./staging/src/k8s.io/component-base/logs/datapol/ -run TestRedact -v`

### Task 3: Wire Redact into configz serialization

**Files:**
- Modify: `staging/src/k8s.io/component-base/configz/configz.go`

- [x] add import for `k8s.io/component-base/logs/datapol`
- [x] in `MarshalJSON()`: call `v.val.DeepCopyObject()` to get a copy
- [x] call `datapol.Redact()` on the deep copy
- [x] `json.Marshal` the redacted copy instead of `v.val`
- [x] write test: register a config with a datapolicy-tagged field, verify `/configz` output contains `"CLASSIFIED"` not the secret
- [x] run tests: `go test ./staging/src/k8s.io/component-base/configz/ -v`

### Task 4: Replace hardcoded masking in kubelet logging

**Files:**
- Modify: `cmd/kubelet/app/server.go`

- [x] add import for `k8s.io/component-base/logs/datapol`
- [x] in `marshalKubeletConfigForLog()`: replace the `StaticPodURLHeader` loop with `datapol.Redact(safe)` (redact the internal type, which carries the datapolicy tags, before converting to versioned — the v1beta1 type is untagged)
- [x] update the function comment to reference datapolicy-based redaction
- [x] verify existing kubelet tests still pass: `go test ./cmd/kubelet/app/ -run TestMarshalKubeletConfigForLog -v` (updated to expect `CLASSIFIED`)
- [x] run tests: `go test ./cmd/kubelet/app/ -count=1 -timeout 120s` (all pass)

### Task 5: Verify acceptance criteria

- [x] verify `/configz` redacts `StaticPodURLHeader` values to `"CLASSIFIED"` (TestConfigzRedactsDatapolicyFields passes)
- [x] verify kubelet logging redacts `StaticPodURLHeader` values to `"CLASSIFIED"` (TestMarshalKubeletConfigForLog passes)
- [x] verify untagged fields are unaffected in both paths (asserted in both tests above)
- [x] run datapol tests: `go test ./staging/src/k8s.io/component-base/logs/datapol/ -v` (PASS)
- [x] run configz tests: `go test ./staging/src/k8s.io/component-base/configz/ -v` (PASS)
- [x] verify no test regressions in affected packages (datapol, configz, cmd/kubelet/app all PASS)

### Task 6: [Final] Update documentation

- [ ] update CLAUDE.md if new patterns discovered
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification:**
- Run the POC test from the issue to confirm the leak is fixed
- `curl -k https://<node>:10250/configz | jq '.kubeletconfig.staticPodURLHeader'` should show `"CLASSIFIED"` values

**Future considerations:**
- Other components using configz (e.g., kube-scheduler, kube-controller-manager) will automatically benefit if their config types have datapolicy tags
- Consider whether `RedactSecrets()` in client-go should align to use `"CLASSIFIED"` instead of `"REDACTED"` (separate discussion)
