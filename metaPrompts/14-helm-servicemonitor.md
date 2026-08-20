# Prompt 14 — Add ServiceMonitor Template to Helm Chart

## Context

The controller exposes Prometheus metrics on `:8080`. Users running Prometheus Operator (very common in production Kubernetes environments) expect a `ServiceMonitor` CRD to wire up scraping automatically. Without it, they must write the `ServiceMonitor` manually or configure Prometheus with static scrape configs.

The Helm chart currently has no `Service` resource (the metrics port is only defined on the `Pod` via `containerPort`), so a `ServiceMonitor` would need a `Service` to back it.

---

## What to Do

### 1. Add a `Service` template for the metrics endpoint

Create `charts/omni-gitops-controller/templates/service.yaml`:

```yaml
{{- if .Values.metrics.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "omni-gitops-controller.fullname" . }}-metrics
  namespace: {{ .Values.namespace }}
  labels:
    {{- include "omni-gitops-controller.labels" . | nindent 4 }}
spec:
  selector:
    {{- include "omni-gitops-controller.selectorLabels" . | nindent 4 }}
  ports:
    - name: metrics
      port: 8080
      targetPort: 8080
      protocol: TCP
{{- end }}
```

### 2. Add a `ServiceMonitor` template

Create `charts/omni-gitops-controller/templates/servicemonitor.yaml`:

```yaml
{{- if and .Values.metrics.enabled .Values.metrics.serviceMonitor.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "omni-gitops-controller.fullname" . }}
  namespace: {{ .Values.metrics.serviceMonitor.namespace | default .Values.namespace }}
  labels:
    {{- include "omni-gitops-controller.labels" . | nindent 4 }}
    {{- with .Values.metrics.serviceMonitor.additionalLabels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  selector:
    matchLabels:
      {{- include "omni-gitops-controller.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: metrics
      interval: {{ .Values.metrics.serviceMonitor.interval | default "30s" }}
      scrapeTimeout: {{ .Values.metrics.serviceMonitor.scrapeTimeout | default "10s" }}
      path: /metrics
{{- end }}
```

### 3. Add values to `values.yaml`

```yaml
metrics:
  enabled: true
  serviceMonitor:
    # Enable if Prometheus Operator is installed in your cluster.
    enabled: false
    # Namespace for the ServiceMonitor. Defaults to the controller namespace.
    namespace: ""
    # Additional labels to add to the ServiceMonitor (e.g. for Prometheus instance selection).
    additionalLabels: {}
    interval: 30s
    scrapeTimeout: 10s
```

### 4. Update the Helm chart lint CI

The `helm-lint.yml` workflow should still pass. Run `helm lint charts/omni-gitops-controller/` locally to verify.

---

## Verification

```bash
helm template omni-gitops-controller charts/omni-gitops-controller/ \
  --set metrics.serviceMonitor.enabled=true | grep -A 20 "kind: ServiceMonitor"
# Should output a valid ServiceMonitor manifest

helm lint charts/omni-gitops-controller/
# Should pass with no errors or warnings
```
