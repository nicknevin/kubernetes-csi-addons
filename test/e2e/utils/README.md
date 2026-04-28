# CSI-Addons end-to-end testing utilities

This directory contains utilities and tools specifically for end-to-end testing, particularly for network fault injection using iptables.

## Files

### Iptables Image Management

- **`Containerfile.iptables`** - Dockerfile for building the iptables manager image with networking tools
- **`Makefile.iptables`** - Specialized Makefile for building/managing the iptables image
- **`prepare-iptables-image.sh`** - Script to build, tag, and verify the iptables image locally
- **`preload-iptables-image.sh`** - **CONSOLIDATED SCRIPT** - Loads iptables images to DR clusters for testing
- **`validate-iptables-fence.sh`** - End-to-end validation of fence/unfence (invoked via `Makefile.iptables` `validate-network-fence`)
- **`emergency-cleanup-iptables.sh`** - Host-side cleanup of leftover iptables fence rules (invoked via `Makefile.iptables` `emergency-cleanup`)

## Usage

### Building Iptables Image

```bash
# From the project root:
make -f test/e2e/utils/Makefile.iptables build-iptables-image

# Or directly:
cd test/e2e/utils
make build-iptables-image
```

### Preparing and Loading Images to Clusters

```bash
# 1. Prepare image locally:
./test/e2e/utils/prepare-iptables-image.sh

# 2. Load to DR clusters:
DR1_CONTEXT=dr1 DR2_CONTEXT=dr2 ./test/e2e/utils/preload-iptables-image.sh

# 3. Verify images are accessible:
VERIFY_ONLY=true ./test/e2e/utils/preload-iptables-image.sh
```

### Validate fencing and full prep flow

```bash
make -f test/e2e/utils/Makefile.iptables validate-network-fence
make -f test/e2e/utils/Makefile.iptables test-e2e-flow   # build + load + validate
```

### Emergency cleanup (leftover REJECT rules / DaemonSet namespaces)

```bash
make -f test/e2e/utils/Makefile.iptables emergency-cleanup
# Or directly, e.g. single context and namespace pattern:
./test/e2e/utils/emergency-cleanup-iptables.sh dr1 'e2e-*'
```

## Troubleshooting

- **Replication test namespaces / VR / NetworkFence leftovers:** run `./hack/clean-replication-e2e-resources.sh` (or `make clean-replication-e2e`) from the project root. See `docs/networkfence-troubleshooting.md` for stuck `NetworkFence` resources.
- **Image build path:** the iptables image is defined only under `test/e2e/utils/Containerfile.iptables`. `hack/run-replication-e2e.sh` builds from that path when the default image tag is used and the image is missing locally.

## Consolidation Notes

This directory was created to organize iptables-related testing utilities that were previously scattered:

**Moved from hack/ to test/e2e/utils/:**

- `hack/preload-iptables-simple.sh` + `hack/preload-iptables-image.sh` → `preload-iptables-image.sh` (consolidated)
- `hack/prepare-iptables-image.sh` → `prepare-iptables-image.sh`
- `build/Containerfile.iptables` → `Containerfile.iptables`
- Main `Makefile` iptables target → `Makefile.iptables`

**Rationale:**

- Iptables functionality is only used for end-to-end network fault injection testing, not the main CSI-Addons controller
- Consolidating duplicate preload scripts reduces maintenance burden
- Organizing test-specific files in test directories improves project structure

The main `Makefile` still provides a compatibility `docker-build-iptables` target that redirects to the new location.
