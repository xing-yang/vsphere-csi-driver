# Docker-Based Build for VDDK Support

This guide explains how to build vsphere-csi-driver binaries with VDDK support on macOS using Docker, similar to velero-plugin-for-vsphere.

## Why Docker Build?

When building on macOS for Linux with VDDK support, you need:
- A Linux build environment (VDDK is Linux-only)
- CGO enabled (for C library integration)
- VDDK libraries available during build

Docker provides a Linux environment on your Mac, enabling VDDK builds without a separate Linux machine.

## Prerequisites

1. **Docker Desktop** installed on macOS
2. **VDDK libraries** extracted to `.libs/vmware-vix-disklib-distrib/`

## Quick Start

```bash
# 1. Extract VDDK (one-time setup)
mkdir -p .libs
cd .libs
tar -xzf ~/Downloads/VMware-vix-disklib-8.0.*.tar.gz
cd ..

# 2. Build with VDDK support
make docker-build-csi

# 3. Verify the binary
make verify-vddk-binary

# 4. Your binary is ready!
ls -lh .build/bin/vsphere-csi.linux_amd64
```

## Available Targets

```bash
make docker-build-csi       # Build CSI driver with VDDK
make docker-build-syncer    # Build syncer (no VDDK needed)
make docker-build-all       # Build both CSI driver and syncer
make verify-vddk-binary     # Check if binary has VDDK support
```

## How It Works

The `docker-build-csi` target:

1. **Checks for VDDK**: Verifies `.libs/vmware-vix-disklib-distrib/lib64` exists
2. **Starts Linux container**: `docker run --platform linux/amd64 golang:1.21`
3. **Mounts your code**: `-v $(pwd):/workspace`
4. **Mounts VDDK libs**: `-v .libs/vmware-vix-disklib-distrib:/usr/local/vmware-vix-disklib-distrib`
5. **Enables CGO**: Sets `CGO_ENABLED=1` with appropriate flags
6. **Builds in Linux**: Compiles with `go build` inside the container
7. **Outputs binary**: Creates `.build/bin/vsphere-csi.linux_amd64` with VDDK support

## Comparing Build Methods

| Build Method | Platform | VDDK Support | Use Case |
|-------------|----------|--------------|----------|
| `make build-csi` on macOS | macOS → Linux | ❌ No (stubs) | Development, quick iterations |
| `make build-csi` on Linux | Linux → Linux | ✅ Yes (if VDDK installed) | Production builds, CI/CD |
| `make docker-build-csi` on macOS | macOS → Linux | ✅ **Yes** (via Docker) | **Testing on Linux from Mac** |

## Example Workflow

### Development on macOS

```bash
# Quick development iteration (no VDDK)
make build-csi
# Binary: .build/bin/vsphere-csi.darwin_arm64 (stubs only)

# For Linux testbed (with VDDK)
make docker-build-csi
# Binary: .build/bin/vsphere-csi.linux_amd64 (real VDDK!)
```

### Deploy to Linux Testbed

```bash
# Build with VDDK on Mac
make docker-build-csi

# Copy to testbed
scp .build/bin/vsphere-csi.linux_amd64 testbed:/opt/csi/

# On testbed, deploy and test CBT operations
ssh testbed
cd /opt/csi
# This binary now has REAL VDDK support!
```

## Verifying VDDK Support

After building, verify your binary has VDDK:

```bash
$ make verify-vddk-binary

Verifying VDDK support in binary...
Binary info:
.build/bin/vsphere-csi.linux_amd64: ELF 64-bit LSB executable, x86-64, dynamically linked

Build info:
  CGO_ENABLED=1
  GOARCH=amd64
  GOOS=linux

✓ Binary is dynamically linked with VDDK
```

## Troubleshooting

### Error: "VDDK libraries not found"

```bash
ERROR: VDDK libraries not found at .libs/vmware-vix-disklib-distrib/lib64
```

**Solution:** Extract VDDK to the correct location:
```bash
mkdir -p .libs
cd .libs
tar -xzf /path/to/VMware-vix-disklib-8.0.*.tar.gz
ls -la vmware-vix-disklib-distrib/lib64  # Should contain libvixDiskLib.so
```

### Error: Docker image pull fails

```bash
Unable to find image 'golang:1.21' locally
Error response from daemon: Get https://registry-1.docker.io/...
```

**Solution:** Ensure Docker Desktop is running and you have internet access.

### Build is slow on first run

This is normal! Docker needs to:
- Pull the `golang:1.21` image (~800MB, one-time)
- Download Go module dependencies (cached for subsequent builds)

Subsequent builds will be much faster.

## Advanced: Custom Docker Image

To use a specific Go version:

```bash
make docker-build-csi DOCKER_GO_VERSION=1.22
```

## Comparison with velero-plugin-for-vsphere

Both projects use the same approach:

**velero-plugin-for-vsphere:**
```bash
docker run --platform linux/amd64 \
  -v $(pwd)/.libs/vmware-vix-disklib-distrib:/usr/local/vmware-vix-disklib-distrib:delegated \
  -e CGO_ENABLED=1 \
  golang:1.23 ./hack/build.sh
```

**vsphere-csi-driver:**
```bash
docker run --platform linux/amd64 \
  -v $(pwd)/.libs/vmware-vix-disklib-distrib:/usr/local/vmware-vix-disklib-distrib:ro \
  -e CGO_ENABLED=1 \
  golang:1.21 go build ...
```

Same pattern, same results! 🎉

## Building Container Images

In addition to building binaries, you can build complete container images with VDDK support:

### Build Container Images with VDDK

```bash
# Build both CSI driver (with VDDK) and syncer images
make docker-images-vddk

# Or build individually
make docker-image-csi-vddk    # CSI driver with VDDK
make docker-image-syncer      # Syncer (no VDDK needed)
```

**Result:**
- CSI Driver: `localhost:5000/vsphere-csi-driver:latest` (405MB with VDDK)
- Syncer: `localhost:5000/vsphere-syncer:latest` (131MB)

### Customize Image Registry and Tag

```bash
# Use your own registry
IMAGE_REGISTRY=myregistry.io IMAGE_TAG=v3.0.0-vddk make docker-images-vddk

# Result:
# - myregistry.io/vsphere-csi-driver:v3.0.0-vddk
# - myregistry.io/vsphere-syncer:v3.0.0-vddk
```

### Push to Registry

```bash
# Set your registry and push
IMAGE_REGISTRY=myregistry.io make docker-images-vddk docker-push
```

### Deploy to Kubernetes

Update your deployment to use the new images:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vsphere-csi-controller
spec:
  template:
    spec:
      containers:
      - name: vsphere-csi-controller
        image: myregistry.io/vsphere-csi-driver:v3.0.0-vddk
        # This image has full VDDK/CBT support!
      
      - name: vsphere-syncer
        image: myregistry.io/vsphere-syncer:v3.0.0-vddk
```

### Available Targets

| Target | Description |
|--------|-------------|
| `make docker-image-csi-vddk` | Build CSI driver with VDDK support |
| `make docker-image-csi` | Build CSI driver without VDDK |
| `make docker-image-syncer` | Build syncer |
| `make docker-images-vddk` | Build both (CSI with VDDK + syncer) |
| `make docker-images` | Build both without VDDK |
| `make docker-push` | Push images to registry |
| `make docker-images-list` | List built images |

## Related Documentation

- [VDDK-SETUP.md](../VDDK-SETUP.md) - Complete VDDK setup guide
- [velero-plugin-for-vsphere Makefile](https://github.com/vmware-tanzu/velero-plugin-for-vsphere/blob/main/Makefile) - Original inspiration

