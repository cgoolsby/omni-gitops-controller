# Prompt 13 — Document `machineExtensions` and `kernelArgs` + Add Example

## Context

`MachineSetSpec.MachineExtensions` and `MachineSetSpec.KernelArgs` are fully
implemented (machine-set-level `ExtensionsConfiguration`, per-machine
`KernelArgs` at the schematic level, prune-on-clear, cleanup on eviction and
teardown) — but the README's Spec Fields table doesn't mention either field, and
no example demonstrates them.

## What to Do

### 1. README spec table

In `README.md` under the Spec Fields table (`## OmniCluster API Reference`), add
rows for `machineExtensions[]` and `kernelArgs[]` for both `spec.controlPlane`
and `spec.workers[]`, after the `configPatches` rows:

- `machineExtensions[]` — Talos system extension IDs (e.g.
  `siderolabs/nvidia-open-gpu-kernel-modules`). Creates/updates an
  `ExtensionsConfiguration` scoped to the machine set; clearing the list deletes
  it so the schematic reverts.
- `kernelArgs[]` — extra kernel args applied at the **schematic** level (active
  during maintenance/install boot, e.g. `libata.force=noncq`). Clearing the list
  removes the per-machine `KernelArgs` resource; evicted machines also release
  theirs.

### 2. `examples/with-extensions-and-kernel-args.yaml`

New example: single CP node plus a `gpu` worker pool with NVIDIA extensions
(`siderolabs/nvidia-open-gpu-kernel-modules`, `siderolabs/nvidia-container-toolkit`),
`kernelArgs: [iommu=pt, intel_iommu=on]`, and a `configPatches` entry loading the
nvidia kernel modules. Include a header comment explaining when to use each
field. **The spec must satisfy current CRD validation**: `kubernetesVersion`
matching `^\d+\.\d+\.\d+$`, `talosVersion` matching `^v\d+\.\d+\.\d+(-...)?$`,
CP `replicas >= 1`, `matchLabels` only if matchExpressions support hasn't
landed (check `api/v1alpha1/types.go` — if the matchExpressions CEL rejection
was removed, either form is fine; keep `matchLabels` for simplicity).

### 3. Examples table

Add the new file to the README's Examples table with a one-line description.

## Verification

```bash
grep -n "machineExtensions\|kernelArgs" README.md
python3 -c "import yaml,sys; yaml.safe_load(open('examples/with-extensions-and-kernel-args.yaml'))"
```

## Constraints

- Do NOT commit. Do NOT touch `metaPrompts/` or unrelated files.

## Acceptance Criteria

- README rows present; example file valid YAML and CRD-conformant; go gates
  pass (unchanged).
