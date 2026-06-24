# Promote sample-device-plugin e2e test image to 1.8.0

## Implementation Steps

### Task 1: Promote sample-device-plugin e2e test image to 1.8.0

- [ ] Bump the sample-device-plugin e2e test image from 1.7 to 1.8.0. Search for ALL references to sample-device-plugin:1.7 across the repo and update them to sample-device-plugin:1.8.0. Common locations: test/e2e/testing-manifests/sample-device-plugin/*.yaml (hardcoded image refs). Use grep to find every occurrence.
