# Prompt 05 — Align Helm Chart version with appVersion

## Context

`charts/omni-gitops-controller/Chart.yaml` currently shows:

```yaml
version: 0.1.2
appVersion: "0.1.12"
```

The release workflow (`.github/workflows/release.yml`) updates both to the tag
version on release, so this is historical drift from before that script existed.
For a simple single-application chart, keeping both identical is the clearest
convention.

## What to Do

1. In `Chart.yaml`, set `version: 0.1.12` to match `appVersion: "0.1.12"`.
2. Confirm the release workflow's sed lines update **both** fields to the same
   tag version (they should already; fix if not).
3. Add a short note to the README's contributing/development section (or near
   the Helm install instructions) stating that chart `version` and `appVersion`
   are kept identical and are bumped together by the release workflow.

## Verification

```bash
grep -E "^version:|^appVersion:" charts/omni-gitops-controller/Chart.yaml
# version: 0.1.12 / appVersion: "0.1.12"
helm lint charts/omni-gitops-controller/
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- Versions aligned; `helm lint` passes; go gates pass.
