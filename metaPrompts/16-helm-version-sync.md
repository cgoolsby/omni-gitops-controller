# Prompt 16 — Fix Helm Chart Version / AppVersion Sync

## Context

`charts/omni-gitops-controller/Chart.yaml` currently shows:

```yaml
version: 0.1.2
appVersion: "0.1.12"
```

The Helm chart version (`version`) has lagged significantly behind the application version (`appVersion`). The release workflow does update both, but the divergence suggests they have not been kept in lockstep and may have gotten out of sync at some point.

In the Helm ecosystem:
- `appVersion` = the version of the application being packaged (the controller binary)
- `version` = the version of the chart itself

For simple single-application charts (like this one), keeping both at the same value is the simplest convention and avoids confusion. Users searching for `omni-gitops-controller` in a Helm registry expect the chart version to correspond to the app version.

---

## What to Do

### 1. Align the versions

Update `Chart.yaml` to set both to the current app version:

```yaml
version: 0.1.12
appVersion: "0.1.12"
```

### 2. Update the release workflow to keep them in sync

The release script in `.github/workflows/release.yml` already updates both:

```bash
VERSION="${GITHUB_REF_NAME#v}"
sed -i "s/^appVersion:.*/appVersion: \"${VERSION}\"/" charts/omni-gitops-controller/Chart.yaml
sed -i "s/^version:.*/version: ${VERSION}/" charts/omni-gitops-controller/Chart.yaml
```

This is correct — both are updated to the same tag version on every release. The issue is historical drift before this script was in place.

### 3. Consider a lockstep policy

Document in `CONTRIBUTING.md` (or the README Contributing section) that chart `version` and `appVersion` are always kept identical. When a PR bumps the chart for non-code reasons (e.g., adding a new template), both should still be bumped together.

---

## Verification

```bash
helm show chart oci://ghcr.io/cgoolsby/charts/omni-gitops-controller --version 0.1.12
# version and appVersion should both be 0.1.12

# After the local fix:
grep -E "^version:|^appVersion:" charts/omni-gitops-controller/Chart.yaml
# version: 0.1.12
# appVersion: "0.1.12"
```

Run `helm lint charts/omni-gitops-controller/` to confirm no structural issues.
