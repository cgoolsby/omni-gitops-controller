# Prompt 08 — CRD-Level Validation Hardening

## Context

The `OmniCluster` CRD accepts specs that the controller cannot honour or that are
silently dangerous:

- `controlPlane.replicas` has no minimum. A runtime guard exists only on the
  scale-*down* path; `replicas: 0` on a new cluster is accepted by the API server.
- `kubernetesVersion` / `talosVersion` have no format validation; typos surface
  only as opaque Omni errors at reconcile time.
- `machineSelector.matchExpressions` is accepted but **silently ignored** by
  `SelectAvailableMachines` (only `matchLabels` is processed). Until
  matchExpressions support is implemented (tracked separately in
  `metaPrompts/05-fix-matchexpressions.md` — do NOT implement it here), specs
  using it should be rejected at admission instead of mis-selecting machines.
- Worker `name` has no charset/length constraints despite being embedded in Omni
  resource IDs.

## What to Do

In `api/v1alpha1/types.go`, add kubebuilder markers:

1. On `MachineSetSpec.Replicas`: `+kubebuilder:validation:Minimum=0`.
2. On the `OmniClusterSpec` struct, a CEL rule enforcing the control-plane
   minimum:

   ```go
   // +kubebuilder:validation:XValidation:rule="self.controlPlane.replicas >= 1",message="controlPlane.replicas must be at least 1"
   ```

   (Defaulting runs before validation, so an omitted `replicas` defaults to 1 and
   passes.)
3. On `OmniClusterSpec.KubernetesVersion`:
   `+kubebuilder:validation:Pattern=` for `^\d+\.\d+\.\d+$`.
4. On `OmniClusterSpec.TalosVersion`: pattern `^v\d+\.\d+\.\d+(-[A-Za-z0-9.]+)?$`
   (allows `-rc.N` style suffixes).
5. On the `MachineSetSpec` struct, a CEL rule rejecting matchExpressions for now:

   ```go
   // +kubebuilder:validation:XValidation:rule="!has(self.machineSelector.matchExpressions) || size(self.machineSelector.matchExpressions) == 0",message="machineSelector.matchExpressions is not supported yet; use matchLabels"
   ```

   Add a short code comment noting this rule should be removed when
   matchExpressions support lands.
6. On `WorkerMachineSetSpec.Name`: `+kubebuilder:validation:MaxLength=63` and
   pattern `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`.

Then regenerate:

```bash
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest
controller-gen object paths="./api/..."
controller-gen crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=config/crd/bases
cp config/crd/bases/omni.gitops.dev_omniclusters.yaml charts/omni-gitops-controller/crds/
```

Verify the example manifests in `examples/` and `config/samples/` still satisfy
the new rules (fix the examples only if one violates a rule).

## Constraints

- Do NOT implement matchExpressions support — only reject it.
- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- `gofmt -l .` clean; `go vet ./...`, `go build ./...`, `go test ./...` pass.
- Regenerated CRD contains the CEL rules and patterns; chart copy matches
  `config/crd/bases/` exactly.
