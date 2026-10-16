# Volume replication POC on a Supervisor (M1, fake SnapService)

These steps set up csi-addons VolumeReplication on one Supervisor. The CSI
driver uses an in-memory fake SnapService with two simulated sites,
`site-a` (this Supervisor) and `site-b`. The design (`volume-replication-poc.md`)
and the test report (`volume-replication-poc-testing.md`) are kept outside this
repository.

## Limitations

- Replication state lives inside the CSI controller process. It is lost when
  the controller restarts or leadership moves to the other replica. A primary
  VR then shows `Replicating=False` with the message "protection group ... does
  not exist". (`Degraded` stays `False`, because csi-addons sets it only from
  promote, demote and resync results.) Delete the VolumeReplication objects and
  create them again.
- No data is replicated, and `site-b` exists only inside the fake.
- WCP manages the CSI deployment. A Supervisor upgrade, or a WCP reconcile,
  can revert the changes below.
- Demote checks VolumeAttachments only. Volumes attached to VM Service VMs
  are not detected yet.
- The sidecar can read secrets in all namespaces.

## Prerequisites

- Root SSH to a Supervisor control plane VM (the password comes from
  `/usr/lib/vmware-wcp/decryptK8Pwd.py` on vCenter). There are two identities
  on the VM:
  - The default kubeconfig (`supervisor-admin`) can patch the CSI deployment
    and ConfigMap, and create PVCs, pods and VRs in namespaces.
  - On vSphere 9 it cannot create namespaces, CRDs or ClusterRoles, so the
    csi-addons install and the RBAC below use `/etc/kubernetes/admin.conf`.
- A vsphere-csi image built from this branch. For example:
  `make driver-image`, then push it to a registry the Supervisor control plane
  can pull from.
- Network access from the control plane to `quay.io`, or copies of
  `quay.io/csiaddons/k8s-controller:v0.15.1` and
  `quay.io/csiaddons/k8s-sidecar:v0.15.1` in a reachable registry.
- Host ports 9070 (sidecar), 9071 and 9072 (csi-addons controller health and
  metrics) free on the control plane VMs. Both pods use the host network,
  because control plane VMs have no pod network.

## 1. Install the csi-addons controller and CRDs

From the repository root:

Render the manifests on a machine with internet access, copy them to the
control plane VM, and apply them with the admin kubeconfig:

```sh
kubectl kustomize manifests/supervisorcluster/replication-poc/csi-addons-controller > csi-addons-controller.yaml
# On the control plane VM:
kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f csi-addons-controller.yaml
kubectl --kubeconfig /etc/kubernetes/admin.conf -n csi-addons-system rollout status deploy/csi-addons-controller-manager
```

## 2. Update the CSI controller

Patch the live objects instead of applying the rendered manifests. The
manifests in this repo may not match the Supervisor's version.

```sh
cd manifests/supervisorcluster/replication-poc
kubectl --kubeconfig /etc/kubernetes/admin.conf apply -f csi-addons-rbac.yaml
kubectl -n vmware-system-csi patch configmap csi-feature-states --patch-file feature-states-patch.yaml
kubectl -n vmware-system-csi patch deployment vsphere-csi-controller --patch-file controller-patch.yaml
kubectl -n vmware-system-csi set image deployment/vsphere-csi-controller \
  vsphere-csi-controller=<registry>/vsphere-csi:<tag>
kubectl -n vmware-system-csi rollout status deploy/vsphere-csi-controller
```

To review the full result first, run
`kubectl kustomize manifests/supervisorcluster/replication-poc`.

## 3. Verify

```sh
# The driver logs that it serves the csi-addons services.
kubectl -n vmware-system-csi logs deploy/vsphere-csi-controller -c vsphere-csi-controller \
  | grep -E "csi-addons|fake SnapService"
# The sidecar registers a CSIAddonsNode with the replication capability.
kubectl get csiaddonsnodes -A -o yaml | grep -A8 capabilities
```

## 4. Try it

Create a VolumeReplicationClass. This is cluster-scoped, so an admin creates
it. The class **must** reference a Secret, even if the backend needs no
credentials. The csi-addons v0.15.1 sidecar always looks the Secret up, and
fails every request with `resource name may not be empty` when there is none:

```sh
kubectl --kubeconfig /etc/kubernetes/admin.conf -n vmware-system-csi \
  create secret generic vsphere-replication-secret --from-literal=note=placeholder
```

```yaml
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplicationClass
metadata:
  name: vsphere-replication
spec:
  provisioner: csi.vsphere.vmware.com
  parameters:
    targetSite: site-b
    schedulingInterval: 5m
    replication.storage.openshift.io/replication-secret-name: vsphere-replication-secret
    replication.storage.openshift.io/replication-secret-namespace: vmware-system-csi
```

The class parameters cannot be changed after creation. To change them, delete
and recreate the class.

Create a VolumeReplication for an existing block PVC in a Supervisor
namespace:

```yaml
apiVersion: replication.storage.openshift.io/v1alpha1
kind: VolumeReplication
metadata:
  name: demo-vr
  namespace: <namespace>
spec:
  volumeReplicationClass: vsphere-replication
  replicationState: primary
  dataSource:
    apiGroup: ""
    kind: PersistentVolumeClaim
    name: <pvc>
```

Each step below lists the result you should see:

| Step | Command | Result |
|---|---|---|
| Enable | create the VR above | `status.state: Primary`, condition `Completed=True` |
| Demote, attached | `kubectl -n <namespace> patch vr demo-vr --type merge -p '{"spec":{"replicationState":"secondary"}}'` while a pod uses the PVC | `FailedPrecondition`: the volume is attached |
| Demote | stop the pod, keep the VR `secondary` | `status.state: Secondary`. Resync reports not ready, because the simulated `site-b` is not promoted |
| Promote | set `replicationState: primary` | `status.state: Primary` again |
| Disable | delete the VR | the VR is removed, the protection group is deleted, the PVC is unchanged |

Logs to follow:

```sh
kubectl -n vmware-system-csi logs deploy/vsphere-csi-controller -c csi-addons -f
kubectl -n vmware-system-csi logs deploy/vsphere-csi-controller -c vsphere-csi-controller -f | grep -i replication
kubectl -n csi-addons-system logs deploy/csi-addons-controller-manager -f
```

## Two Supervisors with the shared mock server

`cmd/fake-snapservice` serves the fake SnapService over REST for several
sites. When both Supervisors use it, they share protection group state. That
lets you test the multi-cluster workflow:

- copying the PV and PVC to site B with the same `volumeHandle`
- demote on A, then promote on B, then resync on A
- forced promote while site A is down
- how site B's syncer handles a PV whose CNS volume doesn't exist yet

No data is replicated. The mock reports recovered disks that don't exist, so
set `X_CSI_REPLICATION_SKIP_VOLUME_REGISTRATION=true` to stop promote from
registering them with CNS. Pods on site B still can't attach the volume.

1. Run the mock server on a host that both Supervisor control planes can reach:

   ```sh
   go build -o fake-snapservice ./cmd/fake-snapservice
   ./fake-snapservice --listen :8080 --sites site-a,site-b   # add --tls-cert/--tls-key for HTTPS
   ```

2. Complete steps 1 and 2 on both Supervisors. Then switch each one to the
   REST backend. Use `site-a` on Supervisor A and `site-b` on Supervisor B:

   ```sh
   kubectl -n vmware-system-csi set env deploy/vsphere-csi-controller -c vsphere-csi-controller \
     X_CSI_REPLICATION_BACKEND=rest \
     X_CSI_REPLICATION_ENDPOINT=http://<mock-host>:8080/site-a \
     X_CSI_REPLICATION_SITE=site-a \
     X_CSI_REPLICATION_CLUSTER=<vsphere-cluster-moid> \
     X_CSI_REPLICATION_SKIP_VOLUME_REGISTRATION=true
   ```

   The endpoint, site, cluster and credentials can also be set in a Secret
   referenced by the VolumeReplicationClass. Use the keys `endpoint`, `site`,
   `cluster`, `username`, `password` and `insecureSkipVerify`. Secret values
   take precedence over the environment variables.

3. Create the VolumeReplicationClass on both Supervisors. On A use
   `targetSite: site-b`. On B use `targetSite: site-a`.

4. On A, create the PVC and a VR with `replicationState: primary`.

5. On B, recreate the PV and PVC with the same volume handle, then create a VR
   with `replicationState: secondary`:

   ```sh
   # On A:
   kubectl get pv <pv> -o json | jq '
     del(.metadata.uid, .metadata.resourceVersion, .metadata.creationTimestamp,
         .metadata.annotations, .metadata.finalizers, .status)
     | .spec.persistentVolumeReclaimPolicy = "Retain"
     | .spec.claimRef = {namespace: "<namespace>", name: "<pvc>"}' > pv-b.json
   # Edit spec.nodeAffinity if the zones differ on B.
   # On B:
   kubectl apply -f pv-b.json
   # Then create the PVC with spec.volumeName: <pv> and the same storage class and size.
   ```

   Expected result: the VR on B is `Secondary`, and becomes not degraded once
   resync reports ready.

6. Planned migration:
   1. Stop the workload on A.
   2. Set the VR on A to `secondary`.
   3. Set the VR on B to `primary`.

   Expected result: B becomes `Primary`, and the resync on A reports ready.

7. Unplanned failover:
   1. Run `curl -X POST "http://<mock-host>:8080/admin/sites/site-a?down=true"`.
   2. Set the VR on B to `primary`.

   Expected result: the first promote fails with `FailedPrecondition`, and the
   controller's forced retry succeeds. Bring A back with `down=false`, then set
   its VR to `secondary`.

To see the protection groups of all sites, run
`curl http://<mock-host>:8080/admin/protection-groups`. When the mock was
started with `--auto-sync=false`, `POST /admin/sync` completes the pending
replication.

## Revert

```sh
kubectl delete vr --all -A
kubectl -n vmware-system-csi patch configmap csi-feature-states --type merge -p '{"data":{"volume-replication":"false"}}'
# Restore the original CSI image and remove the csi-addons container and env vars,
# for example with `kubectl -n vmware-system-csi rollout undo deploy/vsphere-csi-controller`.
kubectl --kubeconfig /etc/kubernetes/admin.conf delete vrc --all
kubectl --kubeconfig /etc/kubernetes/admin.conf -n vmware-system-csi delete secret vsphere-replication-secret
kubectl --kubeconfig /etc/kubernetes/admin.conf delete -f csi-addons-rbac.yaml
kubectl --kubeconfig /etc/kubernetes/admin.conf delete -f csi-addons-controller.yaml
```
