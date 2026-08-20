# Prompt 15 — Add Cosign Image Signing to Release Workflow

## Context

The release workflow (`.github/workflows/release.yml`) already has:
- `id-token: write` permission (required for keyless cosign signing via OIDC)
- A comment mentioning cosign

However, cosign is **never actually called** — the image is pushed to `ghcr.io` unsigned. Image signing with cosign allows users to verify the image provenance before deploying, which matters for supply chain security in production environments.

Keyless signing (via GitHub Actions OIDC) requires no key management and is the recommended approach for open-source projects.

---

## What to Do

### 1. Install cosign in the release workflow

After the `Build and push Docker image` step, add:

```yaml
- name: Install cosign
  uses: sigstore/cosign-installer@v3

- name: Sign the published Docker image
  env:
    DIGEST: ${{ steps.build-push.outputs.digest }}
    TAGS: ${{ steps.meta.outputs.tags }}
  run: |
    images=""
    for tag in ${TAGS}; do
      images+="${tag}@${DIGEST} "
    done
    cosign sign --yes ${images}
```

Note: the `docker/build-push-action` step needs an `id` field to expose its outputs:
```yaml
- name: Build and push Docker image
  id: build-push   # add this
  uses: docker/build-push-action@v7
  ...
```

### 2. Optionally generate and attach an SBOM

While cosign is available, generate a CycloneDX SBOM using `syft` and attach it to the image:

```yaml
- name: Generate SBOM
  uses: anchore/sbom-action@v0
  with:
    image: ghcr.io/${{ github.repository }}@${{ steps.build-push.outputs.digest }}
    format: cyclonedx-json
    output-file: sbom.cyclonedx.json

- name: Attach SBOM to image
  run: |
    cosign attach sbom \
      --sbom sbom.cyclonedx.json \
      ghcr.io/${{ github.repository }}@${{ steps.build-push.outputs.digest }}
```

### 3. Document verification in README

Add a section or a note under `## Quick Start`:

```bash
# Verify the image signature before deploying
cosign verify \
  --certificate-identity-regexp="https://github.com/cgoolsby/omni-gitops-controller/.github/workflows/release.yml.*" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  ghcr.io/cgoolsby/omni-gitops-controller:<version>
```

---

## Verification

After a release tag is pushed:
```bash
cosign verify \
  --certificate-identity-regexp="https://github.com/cgoolsby/omni-gitops-controller/.github/workflows/release.yml.*" \
  --certificate-oidc-issuer="https://token.actions.githubusercontent.com" \
  ghcr.io/cgoolsby/omni-gitops-controller:<version>
# Should print: Verification for ghcr.io/... -- The following checks were performed...
```
