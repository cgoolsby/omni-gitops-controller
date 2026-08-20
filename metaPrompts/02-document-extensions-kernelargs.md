# Prompt 02 — Document `machineExtensions` and `kernelArgs` in README + Add Example

## Context

Both `MachineSetSpec.MachineExtensions` and `MachineSetSpec.KernelArgs` are fully implemented:
- `EnsureMachineSetExtensionsConfiguration` / `DeleteExtensionsConfigurationForMachineSet`
- `EnsureMachineKernelArgs` / `DeleteKernelArgsForMachine`
- Prune-on-clear tested in `TestReconcileMachineSet_PrunesExtensionsWhenEmpty` and `TestReconcileMachineSet_PrunesKernelArgsWhenEmpty`

However, the README's **Spec Fields** table (`## OmniCluster API Reference`) does not mention either field. Users have no way to discover these features from the documentation. There is also no example file demonstrating their use.

---

## What to Do

### 1. Update the README spec table

In `README.md`, under `### Spec Fields`, add rows for both fields. Add them after the `configPatches` rows for `spec.controlPlane` and `spec.workers`:

| Field | Type | Required | Default | Description |
|---|---|---|---|---|
| `spec.controlPlane.machineExtensions[]` | `[]string` | no | — | Talos system extension IDs to install (e.g. `siderolabs/nvidia-open-gpu-kernel-modules`). When set, the controller creates/updates an `ExtensionsConfiguration` resource scoped to the machine set. Clearing the list deletes the resource so the schematic reverts. |
| `spec.controlPlane.kernelArgs[]` | `[]string` | no | — | Extra kernel arguments applied at the schematic level (e.g. `libata.force=noncq`). Active during maintenance/install boot, not just the installed system. Clearing the list removes the `KernelArgs` resource. |

Add equivalent rows for the `spec.workers[]` variants.

### 2. Create `examples/with-extensions-and-kernel-args.yaml`

Write a new example file demonstrating both features together:

```yaml
# OmniCluster with Talos system extensions and kernel arguments.
#
# machineExtensions: installs Talos system extensions (e.g. NVIDIA drivers).
#   The controller creates an ExtensionsConfiguration resource scoped to the
#   machine set. Omni rebuilds the schematic automatically.
#
# kernelArgs: extra kernel arguments applied at the schematic level so they
#   are active during maintenance/install boot (before first install).
#   Use for args that must be present before the system boots for the first
#   time (e.g. SATA/NVMe quirks, IOMMU settings).
apiVersion: omni.gitops.dev/v1alpha1
kind: OmniCluster
metadata:
  name: gpu-cluster-example
  namespace: omni-gitops-system
spec:
  kubernetesVersion: "1.31.0"
  talosVersion: "v1.9.0"
  controlPlane:
    replicas: 1
    machineSelector:
      matchLabels: {}
  workers:
    - name: gpu
      replicas: 2
      machineSelector:
        matchLabels:
          omni.sidero.dev/platform: metal
      machineExtensions:
        - siderolabs/nvidia-open-gpu-kernel-modules
        - siderolabs/nvidia-container-toolkit
      kernelArgs:
        - iommu=pt
        - intel_iommu=on
      configPatches:
        - name: gpu-kernel-modules
          inline:
            machine:
              kernel:
                modules:
                  - name: nvidia
                  - name: nvidia_uvm
                  - name: nvidia_drm
                  - name: nvidia_modeset
```

### 3. Add the new example to the examples table in README

In the `## Examples` section, add a row:

| `examples/with-extensions-and-kernel-args.yaml` | System extensions (NVIDIA) and kernel args on a GPU worker pool. |

---

## Verification

- `grep -n "machineExtensions\|kernelArgs" README.md` returns rows in the spec table.
- `ls examples/with-extensions-and-kernel-args.yaml` succeeds.
- The example is valid YAML: `python3 -c "import yaml, sys; yaml.safe_load(sys.stdin)" < examples/with-extensions-and-kernel-args.yaml`
