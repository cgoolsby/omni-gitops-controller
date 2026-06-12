# Prompt 11 — Add Metrics Service + ServiceMonitor to the Helm Chart

## Context

The controller exposes Prometheus metrics on `:8080` (including the custom
`omni_*` metrics added in the previous change), but the chart defines the
metrics port only as a `containerPort` — there is no `Service`, so users running
Prometheus Operator cannot wire up a `ServiceMonitor` without writing manifests
by hand.

## What to Do

### 1. `charts/omni-gitops-controller/templates/service.yaml`

A metrics `Service` gated on `.Values.metrics.enabled`, selecting the controller
pods. **Check `templates/_helpers.tpl` and `deployment.yaml` for the existing
label/selector conventions** (the chart uses a simple
`app: {{ include "omni-gitops-controller.name" . }}` selector — reuse exactly
that; do not invent `fullname`/`selectorLabels` helpers unless they exist).
Port: name `metrics`, port 8080 → targetPort 8080.

### 2. `charts/omni-gitops-controller/templates/servicemonitor.yaml`

`monitoring.coreos.com/v1` ServiceMonitor gated on
`.Values.metrics.enabled` AND `.Values.metrics.serviceMonitor.enabled`, with
configurable `namespace` (default: controller namespace), `additionalLabels`,
`interval` (default `30s`), `scrapeTimeout` (default `10s`), endpoint port
`metrics`, path `/metrics`. Its selector must match the labels on the Service
from step 1.

### 3. `values.yaml`

```yaml
metrics:
  enabled: true
  serviceMonitor:
    enabled: false
    namespace: ""
    additionalLabels: {}
    interval: 30s
    scrapeTimeout: 10s
```

with explanatory comments matching the existing values.yaml comment style.

## Verification

```bash
helm lint charts/omni-gitops-controller/
helm template t charts/omni-gitops-controller/ --set metrics.serviceMonitor.enabled=true \
  | grep -B2 -A15 "kind: ServiceMonitor"
helm template t charts/omni-gitops-controller/ | grep -c "kind: ServiceMonitor"  # 0 when disabled
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `helm lint` passes; ServiceMonitor renders only when enabled; Service selector
  matches the Deployment's pod labels. Go gates pass (unchanged).
