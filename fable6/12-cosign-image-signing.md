# Prompt 12 — Cosign Keyless Image Signing + SBOM in the Release Workflow

## Context

`.github/workflows/release.yml` already has `id-token: write` permission (the
prerequisite for keyless cosign signing via GitHub OIDC) and a comment
mentioning cosign — but cosign is never actually invoked. Images are pushed to
ghcr.io unsigned.

## What to Do

Read `.github/workflows/release.yml` first and adapt to its actual structure.

### 1. Sign the pushed image

- Ensure the `docker/build-push-action` step has an `id` (e.g. `build-push`) so
  its `digest` output is referenceable.
- After the push step:

```yaml
- name: Install cosign
  uses: sigstore/cosign-installer@v3

- name: Sign the published image
  env:
    DIGEST: ${{ steps.build-push.outputs.digest }}
    TAGS: ${{ steps.meta.outputs.tags }}
  run: |
    for tag in ${TAGS}; do
      cosign sign --yes "${tag}@${DIGEST}"
    done
```

(adjust the metadata step id to whatever the workflow actually uses; if there is
no metadata action, sign `ghcr.io/${{ github.repository }}@${DIGEST}`).

### 2. Generate and attach an SBOM

Use `anchore/sbom-action@v0` to produce a CycloneDX JSON SBOM for the pushed
digest, then `cosign attach sbom --sbom sbom.cyclonedx.json <image>@<digest>`.

### 3. Document verification in the README

Near Quick Start / installation, add the `cosign verify` snippet with
`--certificate-identity-regexp` pointing at this repo's release workflow and
`--certificate-oidc-issuer=https://token.actions.githubusercontent.com`.

## Verification

```bash
python3 -c "import yaml,sys; yaml.safe_load(open('.github/workflows/release.yml'))"
```

(Full end-to-end verification requires pushing a release tag — out of scope
here; YAML validity and correct step wiring are the gate.)

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- Workflow YAML valid; signing + SBOM steps reference real step outputs; README
  documents verification. Go gates pass (unchanged).
