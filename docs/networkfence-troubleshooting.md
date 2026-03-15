# NetworkFence Troubleshooting Guide

This guide helps diagnose why NetworkFence operations may not work even when the CRDs exist in the cluster.

## How NetworkFence Works

1. **CSI driver** deploys with a **CSI-Addons sidecar** that implements the NetworkFence gRPC service
2. **Sidecar** creates a **CSIAddonsNode** CR (self-registration) with endpoint, driver name, etc.
3. **CSI-Addons controller** watches CSIAddonsNode, connects to the sidecar via gRPC, and adds the connection to its pool
4. **NetworkFenceClass** (optional) stores provisioner, secret ref, and parameters; when processed, the controller may call `GetFenceClients` to populate CIDRs in CSIAddonsNode status
5. **NetworkFence** controller reconciles NetworkFence CRs: it resolves the driver via NetworkFenceClass (or spec.driver), gets a connection from the pool, verifies `NETWORK_FENCE` capability, and calls `FenceClusterNetwork` or `UnFenceClusterNetwork` on the sidecar

## Prerequisites Checklist

| Requirement | How to Verify |
|-------------|---------------|
| CSI-Addons controller running | `kubectl get pods -A \| grep -i csi-addons` |
| CSIAddonsNode CRs exist | `kubectl get csiaddonsnode -A` |
| CSIAddonsNode in Connected state | `kubectl get csiaddonsnode -A -o wide` |
| Driver advertises NETWORK_FENCE | Check CSIAddonsNode `status.capabilities` for `network_fence.NETWORK_FENCE` |
| Lease exists for driver | `kubectl get lease -A \| grep -E '<driver>-csi-addons'` |
| NetworkFenceClass has secret params | `kubectl get networkfenceclass <name> -o yaml` → `spec.parameters` must include `csiaddons.openshift.io/networkfence-secret-name` and `csiaddons.openshift.io/networkfence-secret-namespace` |

## Diagnostic Commands

### 1. Check CSI-Addons controller

```bash
kubectl get pods -A | grep -i csi-addons
# Controller should be running (e.g. in openshift-storage or csi-addons-system)
```

### 2. Check CSIAddonsNode resources

```bash
kubectl get csiaddonsnode -A
kubectl get csiaddonsnode -A -o yaml
```

Look for:
- `status.state: Connected` — connection to sidecar established
- `status.capabilities` — must include `network_fence.NETWORK_FENCE` for fencing to work
- `status.networkFenceClientStatus` — populated when NetworkFenceClass exists and driver supports `GET_CLIENTS_TO_FENCE`

### 3. Check driver lease (leader election)

```bash
# Replace <provisioner> with your driver name (e.g. rbd.csi.ceph.com)
PROVISIONER="rbd.csi.ceph.com"
kubectl get lease -A | grep "${PROVISIONER}-csi-addons"
```

If no lease exists, the controller cannot find a leader and `GetLeaderByDriver` fails.

### 4. Check NetworkFence status

```bash
kubectl get networkfence -A
kubectl describe networkfence <name>
```

- `status.result: Succeeded` — operation completed
- `status.result: Failed` — check `status.message` for error details
- Empty status — controller may not have reconciled yet (see GenerationChangedPredicate below)

### 5. Check controller logs

```bash
# Replace with your controller pod name/namespace
kubectl logs -f deployment/csi-addons-controller -n <namespace> | grep -i networkfence
```

Common log messages:
- `"failed to get the networkfenceinstance"` — driver/connection/capability issue
- `"leading CSIAddonsNode X for driver Y does not support NetworkFence"` — capability missing
- `"no leader found for driver X"` — lease or connection pool issue
- `"failed to fence cluster network"` — gRPC call to sidecar failed

## Common Failure Modes

### 1. No CSIAddonsNode or not Connected

**Cause:** CSI driver does not run the CSI-Addons sidecar, or sidecar cannot be reached.

**Fix:** Ensure the CSI driver is deployed with the CSI-Addons sidecar and that the sidecar creates CSIAddonsNode CRs. Check driver documentation.

### 2. Driver does not support NETWORK_FENCE

**Cause:** The CSI-Addons sidecar does not advertise the `NETWORK_FENCE` capability.

**Fix:** The storage driver’s CSI-Addons implementation must support NetworkFence. Not all drivers do. Check `status.capabilities` on CSIAddonsNode.

### 3. No lease / no leader

**Cause:** The sidecar has not acquired the lease, or the connection pool has no entry for the leader.

**Fix:** Verify the CSI-Addons sidecar is running and can acquire the lease. Check RBAC for `coordination.k8s.io/leases`.

### 4. NetworkFenceClass missing secret parameters

**Cause:** NetworkFenceClass must have:
- `csiaddons.openshift.io/networkfence-secret-name`
- `csiaddons.openshift.io/networkfence-secret-namespace`

**Fix:** Add these to `spec.parameters` of the NetworkFenceClass.

### 5. No retries on failure (GenerationChangedPredicate)

**Cause:** The NetworkFence controller uses `GenerationChangedPredicate`, so it only reconciles when the resource’s generation changes. Failed reconciliations are not retried automatically.

**Fix:** Trigger a reconcile by updating the NetworkFence (e.g. add/remove an annotation):

```bash
kubectl annotate networkfence <name> csiaddons.openshift.io/retry=$(date +%s) --overwrite
```

### 6. Wrong provisioner name

**Cause:** NetworkFenceClass `spec.provisioner` must match the CSI driver’s provisioner name exactly (e.g. `rbd.csi.ceph.com`).

**Fix:** Confirm the provisioner name from the StorageClass or CSI driver and align NetworkFenceClass.

## Quick Diagnostic Script

```bash
#!/bin/bash
# Run with: ./networkfence-diagnostic.sh <provisioner>
PROVISIONER="${1:-rbd.csi.ceph.com}"

echo "=== CSI-Addons controller ==="
kubectl get pods -A | grep -i csi-addons

echo -e "\n=== CSIAddonsNodes ==="
kubectl get csiaddonsnode -A
kubectl get csiaddonsnode -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}: state={.status.state} capabilities={.status.capabilities}{"\n"}{end}'

echo -e "\n=== Lease for $PROVISIONER ==="
kubectl get lease -A | grep "${PROVISIONER}-csi-addons"

echo -e "\n=== NetworkFences ==="
kubectl get networkfence -A

echo -e "\n=== NetworkFenceClasses ==="
kubectl get networkfenceclass -A
```

## Post-test recovery (L1-E-003)

L1-E-003 unfences by setting `spec.fenceState: Unfenced` (deletion no longer triggers UnfenceClusterNetwork). Recovery typically takes ~2 minutes after unfence (RBD mirror reconnect, controller retry). If the cluster remains degraded or RBD mirroring stays unhealthy (`daemon health: WARNING`, `rbd_support module is not ready`), use these manual steps:

### 1. Verify fence is removed

```bash
kubectl get networkfence -A
# Should be empty or the test's fence should be gone
```

### 2. Check RBD mirror pool status

```bash
# Replace <pool> with your replication pool (e.g. replicapool)
kubectl -n rook-ceph exec deployment/rook-ceph-tools -- rbd mirror pool status <pool>
```

Look for `daemon health: WARNING` or `image health: WARNING`. If present, proceed with recovery.

### 3. Clear OSD blocklist (if Ceph blocked the fenced node)

```bash
kubectl -n rook-ceph exec deployment/rook-ceph-tools -- ceph osd blocklist ls
# If the fenced node IP appears, clear it:
kubectl -n rook-ceph exec deployment/rook-ceph-tools -- ceph osd blocklist clear <blocked-ip>
```

### 4. Restart rbd-mirror daemon (if daemon health is WARNING)

```bash
# Rook: find the rbd-mirror deployment
kubectl -n rook-ceph get deploy -l app=rbd-mirror
kubectl -n rook-ceph rollout restart deployment/<rbd-mirror-deployment>
```

### 5. Re-apply mirror peer (if peer was lost)

If `rbd mirror pool info <pool>` shows no peers or the peer is unhealthy:

```bash
# Create bootstrap token on primary, import on secondary (see Rook/Ceph docs)
kubectl -n rook-ceph exec deployment/rook-ceph-tools -- rbd mirror pool peer bootstrap create <pool> --site-name primary
# Import the token on the secondary cluster
```

### 6. Wait for recovery

After unfence and any cleanup, allow 1–2 minutes for Ceph to re-establish connectivity. Re-check `rbd mirror pool status <pool>` until `daemon health: OK`.

## References

- [networkfence.md](networkfence.md) — NetworkFence usage
- [networkfenceclass.md](networkfenceclass.md) — NetworkFenceClass usage
- [csiaddonsnode.md](csiaddonsnode.md) — CSIAddonsNode lifecycle
