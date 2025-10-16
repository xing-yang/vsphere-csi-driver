# VDDK Setup for Changed Block Tracking

The VMware Virtual Disk Development Kit (VDDK) is required for testing and running Changed Block Tracking (CBT) features in the vSphere CSI Driver.

## Quick Start (macOS Users) 🚀

If you're on macOS and want to build a Linux binary with VDDK support for testing:

```bash
# 1. Install VDDK libraries
mkdir -p .libs
cd .libs
tar -xzf /path/to/VMware-vix-disklib-8.0.*.tar.gz
cd ..

# 2. Build with Docker (creates Linux binary with VDDK support)
make docker-build-csi

# 3. Verify VDDK is included
make verify-vddk-binary

# 4. Deploy to your Linux testbed and test!
```

**Result:** A Linux binary (`.build/bin/vsphere-csi.linux_amd64`) with **full VDDK/CBT support** that you can deploy to your Linux test environment. This works the same way as velero-plugin-for-vsphere!

## Prerequisites

- VMware VDDK 8.0 or later
- Linux development environment (VDDK libraries are Linux-only)

## Installation Steps

### 1. Download VDDK

Download the VDDK tarball from VMware:
- Visit: https://developer.vmware.com/web/sdk/8.0/vddk
- Download the appropriate version for your system (Linux x64 recommended)

### 2. Extract VDDK Libraries

Create the `.libs` directory in the project root and extract the VDDK tarball:

```bash
# From the project root directory
mkdir -p .libs
cd .libs

# Extract the VDDK tarball (replace with your downloaded file)
tar -xzf /path/to/VMware-vix-disklib-<version>.x86_64.tar.gz

# Verify the structure
ls -la vmware-vix-disklib-distrib/lib64
```

Expected directory structure:
```
.libs/
└── vmware-vix-disklib-distrib/
    ├── lib64/
    │   ├── libvixDiskLib.so
    │   └── ... (other libraries)
    └── include/
        └── ... (header files)
```

### 3. Verify Installation

Check if VDDK is properly installed:

```bash
make check-vddk
```

You should see:
```
✓ VDDK found at .libs/vmware-vix-disklib-distrib/lib64
```

## Building with VDDK Support

Once VDDK is installed, the build system will automatically detect it and enable CGO compilation with VDDK support.

### Build Tags and Platform Support

The implementation uses Go build tags to conditionally compile VDDK-dependent code:

- **`controller_cbt_linux.go`** (`//go:build linux && cgo`)
  - Compiled only when targeting Linux AND CGO is enabled
  - Contains actual VDDK integration code
  - Requires VDDK libraries at build and runtime

- **`controller_cbt_stub.go`** (`//go:build !linux || !cgo`)
  - Compiled when NOT Linux OR CGO is disabled
  - Contains stub implementations that return "not supported" errors
  - Allows builds on any platform without VDDK

This approach ensures:
- ✅ Flexible: Can build on any platform (macOS, Linux, Windows)
- ✅ Functional: Full CBT support when built natively on Linux with VDDK
- ✅ Transparent: Build system automatically selects correct implementation

### Build Scenarios

| Build Host | Target OS | VDDK Installed | CGO | Result |
|------------|-----------|----------------|-----|--------|
| Linux | Linux | Yes | ✅ | Full CBT support |
| Linux | Linux | No | ❌ | Builds, CBT returns errors |
| macOS | Linux | N/A | ❌ | Cross-compile, CBT returns errors |
| macOS | macOS | N/A | ❌ | Builds, CBT returns errors |

### Build Binaries

```bash
# Build will automatically use VDDK if available on Linux
make build-csi

# Or build explicitly for Linux (from any platform)
GOOS=linux GOARCH=amd64 make build-csi
```

### Run Unit Tests

```bash
# Run all unit tests (will include CBT tests if VDDK is available)
make unit-test
```

### Run CBT-Specific Tests

```bash
# Run only Changed Block Tracking tests
make test-cbt
```

## Testing Without VDDK

If VDDK is not installed, the build system will automatically disable CGO and skip CBT tests:

```bash
make unit-test
# Output: Running unit tests (without VDDK - CBT tests will be skipped)...
```

### Important: Cross-Compiled Binaries and Testing

**⚠️ Critical Note:** Binaries built on macOS for Linux targets will **NOT** have VDDK support, even when deployed to a Linux environment with VDDK installed.

**Why?** The build tags are evaluated at **compile time**, not runtime:
- When building on macOS with `GOOS=linux`, `CGO_ENABLED=0` is set
- The stub implementation (`controller_cbt_stub.go`) is compiled into the binary
- The real VDDK code (`controller_cbt_linux.go`) is excluded
- This cannot be changed at runtime

**For Real CBT Testing on Linux:**
You must build the binary **on Linux** (or in a Linux Docker container) with VDDK installed.

### Option 1: Docker Build on macOS (Recommended) ✨

The project now includes Docker-based build targets that work on macOS, similar to velero-plugin-for-vsphere:

```bash
# Build CSI driver with VDDK support using Docker
make docker-build-csi

# Build both CSI driver and syncer
make docker-build-all

# Verify the binary has VDDK support
make verify-vddk-binary
```

**How it works:**
- Runs the build inside a `golang:1.21` Linux container
- Mounts your local VDDK libraries into the container
- Sets `CGO_ENABLED=1` and appropriate CGO flags
- Produces a Linux binary with **real VDDK support** that works on Linux testbeds

**Requirements:**
- Docker Desktop installed on macOS
- VDDK libraries at `.libs/vmware-vix-disklib-distrib/`

### Option 2: Build on Actual Linux Machine

```bash
ssh linux-build-server
cd /path/to/vsphere-csi-driver
# Ensure VDDK is installed at .libs/vmware-vix-disklib-distrib/
make build-csi  # Will use CGO_ENABLED=1 and include real VDDK code
```

### Option 3: Manual Docker Command

If you need more control over the Docker build:

```bash
docker run --rm \
  --platform linux/amd64 \
  -e CGO_ENABLED=1 \
  -e GOOS=linux \
  -e GOARCH=amd64 \
  -v $(pwd)/.libs/vmware-vix-disklib-distrib:/usr/local/vmware-vix-disklib-distrib:ro \
  -v $(pwd):/workspace \
  -w /workspace \
  golang:1.21 \
  /bin/sh -c '\
    export CGO_CFLAGS="-I/usr/local/vmware-vix-disklib-distrib/include" && \
    export CGO_LDFLAGS="-L/usr/local/vmware-vix-disklib-distrib/lib64 -lvixDiskLib -ldl" && \
    go build -o .build/bin/vsphere-csi.linux_amd64 cmd/vsphere-csi/main.go'
```

**Binary Identification:**
```bash
# Check if binary has VDDK support
go version -m .build/bin/vsphere-csi.linux_amd64 | grep CGO
# CGO_ENABLED=1 → Has VDDK (built on Linux)
# CGO_ENABLED=0 → Stub only (cross-compiled from macOS)
```

## Troubleshooting

### VDDK Not Found

If you see:
```
WARNING: VDDK not found at .libs/vmware-vix-disklib-distrib/lib64
```

Ensure:
1. The tarball was extracted to the correct location
2. The directory structure matches the expected layout
3. You have the correct version of VDDK (8.0+)

### Linker Errors on macOS

VDDK libraries are Linux-only. If you're developing on macOS:
- CBT tests cannot run locally
- Use Linux CI/CD environments or Docker containers for testing
- The regular build (without VDDK) will still work on macOS

### Runtime Library Loading Issues

If you encounter library loading issues at runtime:

```bash
# Set LD_LIBRARY_PATH to include VDDK libraries
export LD_LIBRARY_PATH=$PWD/.libs/vmware-vix-disklib-distrib/lib64:$LD_LIBRARY_PATH
```

## CI/CD Integration

For CI/CD pipelines, ensure VDDK is available in your build environment:

```yaml
# Example GitHub Actions workflow
steps:
  - name: Download and Setup VDDK
    run: |
      mkdir -p .libs
      # Download VDDK from secure storage
      aws s3 cp s3://your-bucket/VMware-vix-disklib-8.0.tar.gz .libs/
      cd .libs && tar -xzf VMware-vix-disklib-8.0.tar.gz
  
  - name: Run CBT Tests
    run: make test-cbt
```

## References

- [Virtual Disks GitHub Repository](https://github.com/vmware/virtual-disks)
- [VDDK Documentation](https://developer.vmware.com/docs/17476/virtual-disk-development-kit-programming-guide/)
- [Velero vSphere Plugin VDDK Setup](https://github.com/vmware-tanzu/velero-plugin-for-vsphere/blob/main/Makefile#L27)

## Cleaning Up

To remove VDDK libraries from your local development environment:

```bash
make clean-vddk
```

This will delete the `.libs` directory and all VDDK files.

