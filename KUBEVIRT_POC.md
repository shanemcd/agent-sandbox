# KubeVirt Backend POC for Agent Sandbox

## Goal

Add `runtimeBackend: VirtualMachine` to the Sandbox spec so the controller creates KubeVirt VMs instead of Pods. This enables VM-level isolation for AI agent sandboxes on clusters with KubeVirt/OCP Virt installed.

## Architecture

```
Sandbox CR (runtimeBackend: VirtualMachine)
    │
    ├─ controller creates Secret (cloud-init userdata)
    ├─ controller creates VirtualMachine (containerDisk + cloudInitNoCloud)
    │       │
    │       └─ KubeVirt creates VirtualMachineInstance (the running VM)
    │               │
    │               └─ VM boots, cloud-init configures user/SSH/supervisor
    │
    └─ controller watches VM/VMI, maps status back to Sandbox conditions
```

The Pod backend remains the default. Existing Sandboxes without `runtimeBackend` are unaffected.

## Files to Modify

### 1. `api/v1beta1/sandbox_types.go`

Add after the `SandboxOperatingMode` block (~line 148):

```go
type RuntimeBackend string

const (
    RuntimeBackendPod            RuntimeBackend = "Pod"
    RuntimeBackendVirtualMachine RuntimeBackend = "VirtualMachine"
)
```

Add field to `SandboxBlueprint`:

```go
// +kubebuilder:default=Pod
// +kubebuilder:validation:Enum=Pod;VirtualMachine
// +optional
RuntimeBackend RuntimeBackend `json:"runtimeBackend,omitempty"`
```

Then run `make generate` to regenerate deepcopy and CRD YAML.

### 2. `controllers/sandbox_controller.go`

**Constants to add:**

```go
var (
    kubevirtVMGVK = schema.GroupVersionKind{
        Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine",
    }
)
```

**RBAC markers to add:**

```go
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachineinstances,verbs=get;list;watch
```

**`reconcileChildResources()` (line 232):** Branch on `sandbox.Spec.RuntimeBackend`:
- `VirtualMachine`: call `reconcileVirtualMachine()`
- default (`Pod`): call `reconcilePod()` (existing behavior)

**New functions to add:**

| Function | Purpose |
|----------|---------|
| `reconcileVirtualMachine()` | Top-level VM lifecycle: create, resume, read status |
| `createCloudInitSecret()` | Create K8s Secret with cloud-init userdata |
| `createVirtualMachine()` | Build VM spec as `unstructured.Unstructured` and create |
| `buildCloudInitUserdata()` | Generate `#cloud-config` YAML (sandbox user, SSH, supervisor) |
| `updateStatusFromVMI()` | Read VMI phase/IP, map to Sandbox conditions |
| `suspendVirtualMachine()` | Patch `spec.running = false` on the VM |

**`handleSandboxExpiry()` (line 1174):** Add VM + Secret cleanup for `VirtualMachine` backend.

**`SetupWithManager()` (line 1300):** Add `Owns()` for VirtualMachine and Secret with the sandbox label predicate.

## Key Design Decisions

**Unstructured API:** Uses `unstructured.Unstructured` for KubeVirt resources to avoid importing the full `kubevirt.io/api` Go module. Keeps the dependency footprint minimal.

**Image mapping:** `PodTemplate.Spec.Containers[0].Image` is used as the containerDisk image for the VM. This reuses the existing spec structure without adding new fields.

**Cloud-init Secret:** KubeVirt limits inline cloudInitNoCloud userdata to 2048 bytes. The controller creates a Secret and references it via `secretRef`. The Secret gets an ownerReference to the Sandbox for GC.

**Conditions:** The VM reconciler sets Sandbox conditions directly (Ready, Suspended, Finished) based on VMI phase, bypassing the Pod-based `computeConditions()`. A production implementation should refactor conditions into a backend-agnostic interface.

**VMI watch gap:** VMIs are owned by VMs, not Sandboxes, so `Owns()` doesn't catch VMI changes directly. The POC re-reads VMI status on each reconcile triggered by VM changes. Production should add a `Watches()` handler for VMIs that maps via labels.

## Testing

### Prerequisites
- Cluster with KubeVirt installed (CRC with OCP Virt works)
- A containerDisk image (e.g., `quay.io/containerdisks/fedora:latest` for basic testing, or a custom image with `openshell-sandbox` baked in)

### Test manifest

```yaml
apiVersion: agents.x-k8s.io/v1beta1
kind: Sandbox
metadata:
  name: kubevirt-test
spec:
  runtimeBackend: VirtualMachine
  podTemplate:
    spec:
      containers:
      - name: sandbox
        image: quay.io/containerdisks/fedora:latest
```

### Expected behavior

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox kubevirt-test -w
# Should show Ready=True once VMI reaches Running phase

kubectl get vm,vmi
# Should show the VirtualMachine and VirtualMachineInstance

# Suspend
kubectl patch sandbox kubevirt-test --type merge -p '{"spec":{"operatingMode":"Suspended"}}'
# VM should stop, Suspended=True

# Resume
kubectl patch sandbox kubevirt-test --type merge -p '{"spec":{"operatingMode":"Running"}}'
# VM should restart, Ready=True

# Delete
kubectl delete sandbox kubevirt-test
# VM, VMI, and cloud-init Secret should be garbage collected
```

## Reference Implementation

The standalone KubeVirt driver at `github.com/clankrshq/OpenShell` (branch `kubevirt-driver`, crate `openshell-driver-kubevirt`) has a working Rust implementation of the full lifecycle. The cloud-init generation, VM spec construction, and status mapping are directly translatable.

## Current Status

**Phases 1–6 WORKING** on CRC (OCP 4.22 + OCP Virt 4.22), Slack + Vertex inference verified 2026-07-10.

- Controller creates VM + cloud-init Secret from Sandbox CR (`runtimeBackend: VirtualMachine`)
- OpenShell K8s driver sets `runtimeBackend: VirtualMachine`, `OPENSHELL_ENDPOINT`, and `OPENSHELL_SANDBOX_COMMAND`
- When `OPENSHELL_SANDBOX_COMMAND` is set, cloud-init emits a **two-service** topology:
  - `openshell-sandbox.service` → `--mode=network` (netns + proxy only)
  - `sandbox-workload.service` → waits for `/run/openshell/netns`, loads `provider.env`, trusts MITM CA, `nsenter`s, writes `entrypoint.pid`, runs the command as `sandbox`
- bootc Hermes containerDisk (`hermes-sandbox-kubevirt`) includes supervisor sidecar binary + NemoClaw guard patches
- In-cluster OpenShell gateway holds providers/inference (separate from host-local `tot`)
- Sandbox status shows Ready=True with VMI pod IP; Slack Socket Mode + `inference.local` work through the proxy

**Operational notes:**
- Always configure providers/inference on the **in-cluster** gateway (`oc port-forward -n openshell svc/openshell 18080:8080`), not `tot`
- bootc images need tmpfs overlays for `/sandbox` and `/opt/data` (cloud-init runcmd handles this)
- Discord/Signal are disabled in the Slack-focused Hermes image; Signal needs a routable signal-cli endpoint (not `host.containers.internal`)

## Deployment Notes

**Controller image:** Built from `fedora-minimal:44` with statically compiled controller binary. Pushed to CRC internal registry at `image-registry.openshift-image-registry.svc:5000/agent-sandbox-system/agent-sandbox-controller:kubevirt`.

**Container disk image:** Built via OpenShift BuildConfig using `virt-customize` on a Fedora Cloud base qcow2. Current minimal image includes: openssh-server, cloud-init, iproute, nftables, openshell-sandbox binary. Available at `image-registry.openshift-image-registry.svc:5000/openshell-sandboxes/openshell-sandbox-kubevirt:latest`.

**RBAC:** Controller SA needs `kubevirt.io` (virtualmachines, virtualmachineinstances) and core Secrets RBAC. Applied via ClusterRole `agent-sandbox-kubevirt`.

**Build workflow:**
```bash
# Build controller
podman exec -u 1000 -e HOME=/var/home/shanemcd -e GOPATH=/var/home/shanemcd/go \
  -e GOMODCACHE=/var/home/shanemcd/go/pkg/mod -e CGO_ENABLED=0 \
  fedora-toolbox-44 bash -c 'cd /path/to/agent-sandbox && go build -o bin/agent-sandbox-controller ./cmd/agent-sandbox-controller/'

# Build + push image (minimal fedora image wrapping the static binary)
BUILDDIR=$(mktemp -d)
cp bin/agent-sandbox-controller "$BUILDDIR/"
cat > "$BUILDDIR/Containerfile" <<'EOF'
FROM registry.fedoraproject.org/fedora-minimal:44
COPY agent-sandbox-controller /agent-sandbox-controller
USER 65532:65532
ENTRYPOINT ["/agent-sandbox-controller"]
EOF
podman build -t localhost/agent-sandbox-controller:kubevirt "$BUILDDIR"
REGISTRY=default-route-openshift-image-registry.apps-crc.testing
podman push --tls-verify=false localhost/agent-sandbox-controller:kubevirt \
  "$REGISTRY/agent-sandbox-system/agent-sandbox-controller:kubevirt"
oc rollout restart deploy/agent-sandbox-controller -n agent-sandbox-system
```

## In-cluster gateway bootstrap (not `tot`)

The Hermes VM talks to `http://openshell.openshell.svc.cluster.local:8080`. Host CLI default gateway `tot` is a **separate** credential store. After reinstalling the in-cluster gateway, re-apply providers/inference:

```bash
oc port-forward -n openshell svc/openshell 18080:8080

# Vertex inference (project must match a validated ADC quota project)
openshell provider create --gateway-endpoint http://127.0.0.1:18080 \
  --name vertex-prod --type google-vertex-ai --from-gcloud-adc \
  --config VERTEX_AI_PROJECT_ID=itpc-gcp-hcm-pe-eng-claude \
  --config VERTEX_AI_REGION=global
# or: openshell provider update ... if it already exists

openshell inference set --gateway-endpoint http://127.0.0.1:18080 \
  --provider vertex-prod --model claude-opus-4-6

openshell provider list --gateway-endpoint http://127.0.0.1:18080
openshell inference get --gateway-endpoint http://127.0.0.1:18080
```

Also create Slack (and any other) providers on the **same** endpoint. Discord token rotation and Signal (`host.containers.internal`) are out of scope for the VM bake; the Slack-focused image disables both platforms.

## What This Does NOT Cover (Future Work)

- Dedicated `VMTemplate` field on the Sandbox spec (proper alternative to reusing PodTemplate)
- Workspace persistence (PVC-backed `/sandbox` for VMs)
- Warm pool support for VMs (pre-booted VMs ready for allocation)
- vCPU/memory configuration per-sandbox (currently hardcoded to 2 cores / 2Gi)
- GPU passthrough for VMs
- VMI watch gap (VMIs owned by VMs, not Sandboxes; needs a Watches handler for label-based mapping)
