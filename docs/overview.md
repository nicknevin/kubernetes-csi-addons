# Kubernetes CSI-Addons Documentation Index

Here's a comprehensive index of the documentation with descriptions organized by category and hierarchy:

## 📋 Documentation Tree Structure

```
docs/
├── 🔧 Configuration & Deployment
│   ├── ci.md
│   ├── csi-addons-config.md
│   ├── deploy-controller.md
│   └── csiaddonsnode.md
├── 🔒 Security & Operations
│   ├── encryptionkeyrotation.md
│   ├── networkfence.md
│   ├── networkfenceclass.md
│   └── reclaimspace.md
├── 📊 Storage Replication
│   ├── volumereplication.md
│   ├── volumereplicationclass.md
│   ├── volumegroupreplication.md
│   ├── volumegroupreplicationclass.md
│   └── volumegroupreplicationcontent.md
├── 🔍 Monitoring & Health
│   ├── volume-condition.md
│   └── testing-architecture.md
└── 📐 Design Documents
    └── design/
        ├── volumereplication.md
        └── volumegroupreplication.md
```

## 📚 Detailed Documentation Index

### 🔧 **Configuration & Deployment**

#### [ci.md](ci.md)
**GitHub Workflows and Continuous Integration**
- Configures GitHub workflows for multi-architecture builds
- Explains `BUILD_PLATFORMS` variable for AMD64, ARM32/64 platforms
- Details container image building process in CI/CD

#### [csi-addons-config.md](csi-addons-config.md)
**CSI-Addons Operator Configuration**
- ConfigMap-based operator configuration (`csi-addons-config`)
- Settings: reclaim-space-timeout, max-concurrent-reconciles, max-group-pvcs
- Schedule precedence configuration (PVC vs StorageClass priority)

#### [deploy-controller.md](deploy-controller.md)
**CSI-Addons Controller Deployment Guide**
- Command-line arguments and configuration options
- Installation methods for latest and versioned deployments
- RBAC setup, CRDs, and controller manifest deployment

#### [csiaddonsnode.md](csiaddonsnode.md)
**CSIAddonsNode Custom Resource**
- Node-scoped resource for CSI-Addons sidecar discovery
- Driver endpoint configuration and node identification
- Lifecycle management tied to DaemonSet/Deployment

### 🔒 **Security & Operations**

#### [encryptionkeyrotation.md](encryptionkeyrotation.md)
**Encryption Key Rotation Operations**
- `EncryptionKeyRotationJob` for on-demand key rotation
- `EncryptionKeyRotationCronJob` for scheduled operations
- Retry policies, timeouts, and backoff limits configuration

#### [networkfence.md](networkfence.md)
**Network Fencing Operations**
- Cluster-scoped resource for network access control
- CIDR block-based fencing/unfencing operations
- Integration with NetworkFenceClass for configuration

#### [networkfenceclass.md](networkfenceclass.md)
**Network Fence Class Configuration**
- GetFenceClients operation configuration
- Provisioner-specific parameters and secrets
- Status reporting in CSIAddonsNode

#### [reclaimspace.md](reclaimspace.md)
**Volume Space Reclamation**
- `ReclaimSpaceJob` for immediate space reclamation
- `ReclaimSpaceCronJob` for scheduled cleanup operations
- Timeout and retry configuration options

### 📊 **Storage Replication**

#### [volumereplication.md](volumereplication.md)
**Individual Volume Replication**
- Namespaced resource for single PVC replication
- Primary/secondary/resync states management
- DataSource configuration (PVC references)

#### [volumereplicationclass.md](volumereplicationclass.md)
**Volume Replication Class Configuration**
- Cluster-scoped driver configuration parameters
- Secret management for replication operations
- Vendor-specific parameter handling

#### [volumegroupreplication.md](volumegroupreplication.md)
**Volume Group Replication**
- Multi-PVC replication with label selectors
- Group-level replication state management
- Integration with VolumeGroupReplicationClass and Content

#### [volumegroupreplicationclass.md](volumegroupreplicationclass.md)
**Volume Group Replication Class**
- Driver configuration for group replication operations
- Reserved parameter keys for secrets and configuration
- Provisioner-specific settings

#### [volumegroupreplicationcontent.md](volumegroupreplicationcontent.md)
**Volume Group Replication Content**
- Cluster-scoped resource with volume grouping information
- Volume handle lists and group attributes
- References to VolumeGroupReplication resources

### 🔍 **Monitoring & Health**

#### [volume-condition.md](volume-condition.md)
**Volume Health Monitoring**
- NodeGetVolumeStats-based condition reporting
- Abnormal volume condition detection and alerting
- Event generation for PersistentVolumeClaims
- Future enhancements for metrics and annotations

#### [testing-architecture.md](testing-architecture.md)
**Comprehensive Testing Framework Documentation**
- Ginkgo/Gomega BDD testing framework usage
- Controller-runtime envtest integration testing
- Test suite organization and component testing strategies
- Mock-based testing for CSI driver interactions

### 📐 **Design Documents**

#### [design/volumereplication.md](design/volumereplication.md)
**Volume Replication Architecture Design**
- API design rationale and workflow details
- Primary/secondary cluster replication concepts
- VolumeReplication and VolumeReplicationClass relationships
- Disaster recovery use cases and implementation

#### [design/volumegroupreplication.md](design/volumegroupreplication.md)
**Volume Group Replication Architecture**
- Multi-volume group replication design
- Label selector-based PVC grouping
- VGR, VGRC, and VGRContent resource relationships
- Real-time disaster recovery scenarios

---

**Total Documents**: 16 files  
**Categories**: 4 main categories + design documents  
**Testing Directory**: Currently empty  

This documentation covers the complete CSI-Addons ecosystem including deployment, configuration, security operations, storage replication, monitoring, and architectural design principles.