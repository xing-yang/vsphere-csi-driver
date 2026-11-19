# Agent Substrate external volumes on VKS clusters

These manifests let [Agent Substrate](https://github.com/agent-substrate/substrate) use the vSphere
CSI driver (pvCSI) for actor external volumes (`ExternalVolumes` preview feature) in a VKS cluster.

Agent Substrate calls the CSI controller and node services directly, without PVCs, PVs or
VolumeAttachments:

- `ate-api-server` calls CreateVolume, DeleteVolume, ControllerPublishVolume and
  ControllerUnpublishVolume over TCP with mTLS. Publish and unpublish run on every actor resume
  and suspend.
- `atelet` calls NodeStageVolume and NodePublishVolume on the node plugin socket, under
  `/var/lib/ate`.

pvCSI provisions each volume as a Supervisor PVC named `<tkc-uid>-substrate-<actor-uid>-<volume>`,
labeled `cns.vmware.com/agent-substrate=true`, and attaches it through the VM Operator
VirtualMachine of the node, as for any other pvCSI block volume.

## Requirements

- Agent Substrate installed in the `ate-system` namespace, including its pod certificate signers
  (`servicedns.podcert.ate.dev`, `podidentity.podcert.ate.dev`). If Substrate is installed in
  another namespace, update the SPIFFE ID in `envoy-configmap.yaml`.
- The `PodCertificateRequest` and `ClusterTrustBundle` Kubernetes APIs enabled in the VKS cluster.
  They are off by default in Kubernetes 1.36: enable the `ClusterTrustBundle`,
  `ClusterTrustBundleProjection` and `PodCertificateRequest` feature gates on kube-apiserver,
  kube-controller-manager and every kubelet, and `--runtime-config=certificates.k8s.io/v1beta1=true`
  on kube-apiserver. The VKS ClusterClass has no variable for these settings.
- Worker nodes with Linux 6.7 or later (or 6.6.23 or later). Agent Substrate mounts actor root
  filesystems with the overlayfs `lowerdir+` option, which older kernels reject, so no actor can
  start on them. This is a Substrate requirement, not a CSI one.
- pvCSI running in the `vmware-system-csi` namespace. Otherwise, update the namespace in these
  manifests.
- A Supervisor storage class whose datastore is reachable from every worker node. Agent Substrate
  does not request topology and may resume an actor on any worker node.

## Files

| File | Purpose |
|---|---|
| `service.yaml` | TCP Service in front of the controller's Envoy sidecar |
| `envoy-configmap.yaml` | Envoy configuration: mTLS listener that only accepts `ate-api-server` |
| `controller-envoy-patch.yaml` | Adds the Envoy sidecar to the `vsphere-csi-controller` Deployment |
| `node-ate-mount-patch.yaml` | Mounts `/var/lib/ate` with Bidirectional propagation in the `vsphere-csi-node` DaemonSet |
| `csidriverconfig.yaml` | Registers `csi.vsphere.vmware.com` with Agent Substrate |
| `storageclass.yaml` | Example StorageClass for actor templates |

## Setup

pvCSI is managed by the VKS add-on. Patches applied by hand are meant for development and
testing; the add-on may revert them when it reconciles pvCSI.

1. Enable the feature state:

   ```sh
   kubectl -n vmware-system-csi patch configmap internal-feature-states.csi.vsphere.vmware.com \
     --type merge -p '{"data":{"agent-substrate-support":"true"}}'
   ```

2. Create the Service and Envoy configuration. The Service must exist before the sidecar starts,
   because the sidecar's serving certificate is issued for the Service's DNS name.

   ```sh
   kubectl apply -f service.yaml -f envoy-configmap.yaml
   ```

3. Patch the controller and node plugin:

   ```sh
   kubectl -n vmware-system-csi patch deployment vsphere-csi-controller \
     --type strategic --patch-file controller-envoy-patch.yaml
   kubectl -n vmware-system-csi patch daemonset vsphere-csi-node \
     --type strategic --patch-file node-ate-mount-patch.yaml
   ```

   Restart the controller and node pods if they do not roll out on their own, so that they
   reload the feature state.

4. Register the driver and create a StorageClass. The StorageClasses VKS creates for the vSphere
   Namespace storage policies (with an `svStorageClass` parameter) also work.

   ```sh
   kubectl apply -f csidriverconfig.yaml
   kubectl apply -f storageclass.yaml   # after setting svStorageClass
   ```

5. Reference the StorageClass from an actor template volume:

   ```json
   {"name": "data", "external": {"template": {"capacityBytes": "1073741824",
     "storageClassName": "vsphere-substrate-block"}}}
   ```

## Limitations

- Block volumes only for now. vSphere CSI provisions a file volume (vSAN File Services) only for
  multi-node access modes, and Agent Substrate always requests single-node-writer volumes, so
  every actor volume is a block volume.
- Each resume attaches the volume to the worker node's VM through VM Operator and CNS, which
  takes seconds and adds directly to the actor resume latency.
- StorageClass parameters reserved for Kubernetes (`csi.storage.k8s.io/*`) are ignored, and only
  the `ext4` filesystem type is supported.
- Each actor volume is a Supervisor PVC for as long as the actor exists, including while it is
  suspended, and counts against the vSphere Namespace storage quota.
