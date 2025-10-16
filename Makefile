all: build

# Get the absolute path and name of the current directory.
PWD := $(abspath .)
BASE_DIR := $(notdir $(PWD))

# BUILD_OUT is the root directory containing the build output.
export BUILD_OUT ?= .build

# BIN_OUT is the directory containing the built binaries.
export BIN_OUT ?= $(BUILD_OUT)/bin

# DIST_OUT is the directory containting the distribution packages
export DIST_OUT ?= $(BUILD_OUT)/dist

# Compile Go with boringcrypto. This is required to import crypto/tls/fipsonly package.
export GOEXPERIMENT=boringcrypto


################################################################################
##                             VERIFY GO VERSION                              ##
################################################################################
# Go 1.13 required for Go modules.
GO_VERSION_EXP := "go1.13"
GO_VERSION_ACT := $(shell a="$$(go version | awk '{print $$3}')" && test $$(printf '%s\n%s' "$${a}" "$(GO_VERSION_EXP)" | sort | tail -n 1) = "$${a}" && printf '%s' "$${a}")
ifndef GO_VERSION_ACT
$(error Requires Go $(GO_VERSION_EXP)+ for Go module support)
endif
MOD_NAME := $(shell head -n 1 <go.mod | awk '{print $$2}')

################################################################################
##                             VERIFY BUILD PATH                              ##
################################################################################
ifneq (on,$(GO111MODULE))
export GO111MODULE := on
# should not be cloned inside the GOPATH.
GOPATH := $(shell go env GOPATH)
ifeq (/src/$(MOD_NAME),$(subst $(GOPATH),,$(PWD)))
$(warning This project uses Go modules and should not be cloned into the GOPATH)
endif
endif
# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

################################################################################
##                                DEPENDENCIES                                ##
################################################################################
# The Virtual Disk Development Kit (VDDK) is required for Changed Block Tracking.
# Please refer to https://github.com/vmware/virtual-disks#dependency for the dependency.
# To compile and test with VDDK, download the VDDK tarball to .libs/ directory
# (create the directory if it doesn't exist) and extract it.
# NOTE: VDDK is Linux-only, so it will only be used when building for Linux
LIB_DIR := .libs
VDDK_LIBS := $(LIB_DIR)/vmware-vix-disklib-distrib/lib64

# Check if VDDK is available (optional for most builds)
# Only consider VDDK available if:
# 1. The library directory exists AND
# 2. We're building for Linux (VDDK only supports Linux) AND
# 3. We're running on Linux (to avoid cross-compilation issues with CGO)
VDDK_DIR_EXISTS := $(shell test -d $(VDDK_LIBS) && echo "yes" || echo "no")
BUILD_OS := $(shell uname -s | tr '[:upper:]' '[:lower:]')
ifeq ($(GOOS),linux)
ifeq ($(BUILD_OS),linux)
ifeq ($(VDDK_DIR_EXISTS),yes)
VDDK_AVAILABLE := yes
else
VDDK_AVAILABLE := no
endif
else
VDDK_AVAILABLE := no
endif
else
VDDK_AVAILABLE := no
endif

# Verify the dependencies are in place.
.PHONY: deps
deps:
	go mod download && go mod verify

# Check VDDK dependency for CBT tests
.PHONY: check-vddk
check-vddk:
ifeq ($(VDDK_AVAILABLE),no)
	@echo "WARNING: VDDK not found at $(VDDK_LIBS)"
	@echo "To run Changed Block Tracking tests, please:"
	@echo "  1. Download VDDK from https://developer.vmware.com/web/sdk/8.0/vddk"
	@echo "  2. Extract to $(LIB_DIR)/vmware-vix-disklib-distrib/"
	@echo "  3. Ensure $(VDDK_LIBS) contains the library files"
else
	@echo "✓ VDDK found at $(VDDK_LIBS)"
endif

################################################################################
##                                VERSIONS                                    ##
################################################################################
# Ensure the version is injected into the binaries via a linker flag.
# From users' perspective, a tag makes more sense than a commit ID. When we check the
# CSI Driver's version from the log, it's not convenient to see a commit ID, because
# each time we need to find out the related tag so as to confirm the human readable version.
#
# So we only use the commit id as the version for the binaries built from master branch,
# and use the tag as the version for any release branches.
ifeq ($(shell git rev-parse --abbrev-ref HEAD), master)
VERSION := $(shell git log -1 --format=%h)
else
VERSION := $(shell git describe --dirty --always 2>/dev/null)
endif

.PHONY: version
version:
	@echo $(VERSION)

################################################################################
##                                BUILD DIRS                                  ##
################################################################################
.PHONY: build-dirs
build-dirs:
	@mkdir -p $(BIN_OUT)
	@mkdir -p $(DIST_OUT)

################################################################################
##                              BUILD BINARIES                                ##
################################################################################
# Unless otherwise specified the binaries should be built for linux-amd64.
GOOS ?= linux
GOARCH ?= amd64

LDFLAGS := $(shell cat hack/make/ldflags.txt)
LDFLAGS_CSI := $(LDFLAGS) -X "$(MOD_NAME)/pkg/csi/service.Version=$(VERSION)"
LDFLAGS_SYNCER := $(LDFLAGS) -X "$(MOD_NAME)/pkg/syncer.Version=$(VERSION)"

# Set CGO flags for building with VDDK support
ifeq ($(VDDK_AVAILABLE),yes)
BUILD_CGO_ENABLED := 1
BUILD_CGO_CFLAGS := -I$(abspath $(LIB_DIR)/vmware-vix-disklib-distrib)/include
BUILD_CGO_LDFLAGS := -L$(abspath $(VDDK_LIBS)) -lvixDiskLib -ldl -Wl,-rpath,$(abspath $(VDDK_LIBS))
else
BUILD_CGO_ENABLED := 0
BUILD_CGO_CFLAGS :=
BUILD_CGO_LDFLAGS :=
endif

# The CSI binary.
CSI_BIN_NAME := vsphere-csi
CSI_BIN := $(BIN_OUT)/$(CSI_BIN_NAME).$(GOOS)_$(GOARCH)
CSI_BIN_LINUX := $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH)
CSI_BIN_WINDOWS := $(BIN_OUT)/$(CSI_BIN_NAME).windows_$(GOARCH)
build-csi: $(CSI_BIN) 
build-csi-windows: $(CSI_BIN_WINDOWS)
ifndef CSI_BIN_SRCS
CSI_BIN_SRCS := cmd/$(CSI_BIN_NAME)/main.go go.mod go.sum
CSI_BIN_SRCS += $(addsuffix /*.go,$(shell go list -f '{{ join .Deps "\n" }}' ./cmd/$(CSI_BIN_NAME) | grep $(MOD_NAME) | sed 's~$(MOD_NAME)~.~'))
export CSI_BIN_SRCS
endif
$(CSI_BIN): $(CSI_BIN_SRCS)
	CGO_ENABLED=$(BUILD_CGO_ENABLED) CGO_CFLAGS="$(BUILD_CGO_CFLAGS)" CGO_LDFLAGS="$(BUILD_CGO_LDFLAGS)" GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags '$(LDFLAGS_CSI)' -o $(CSI_BIN_LINUX) $<
	@touch $@

$(CSI_BIN_WINDOWS): $(CSI_BIN_SRCS)
	CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build -ldflags '$(LDFLAGS_CSI)' -o $(CSI_BIN_WINDOWS) $<
	@touch $@

################################################################################
##                          DOCKER-BASED BUILD (with VDDK)                    ##
################################################################################
# Docker-based build for creating Linux binaries with VDDK support on any platform
# This runs the build inside a Linux container, enabling VDDK even on macOS

DOCKER_GO_VERSION ?= 1.24.2
DOCKER_IMAGE := golang:$(DOCKER_GO_VERSION)

# Check if VDDK libraries are available for Docker mount
VDDK_FOR_DOCKER := $(shell test -d $(VDDK_LIBS) && echo "yes" || echo "no")

.PHONY: docker-build-csi
docker-build-csi:
ifeq ($(VDDK_FOR_DOCKER),no)
	@echo "ERROR: VDDK libraries not found at $(VDDK_LIBS)"
	@echo "Please install VDDK before running docker-build-csi:"
	@echo "  1. Download VDDK from https://developer.vmware.com/web/sdk/8.0/vddk"
	@echo "  2. Extract to $(LIB_DIR)/vmware-vix-disklib-distrib/"
	@echo ""
	@echo "To build without VDDK support, use: make build-csi"
	@exit 1
endif
	@echo "Building vsphere-csi with VDDK support in Docker container..."
	@echo "  Docker Image: $(DOCKER_IMAGE)"
	@echo "  VDDK Path: $(abspath $(LIB_DIR)/vmware-vix-disklib-distrib)"
	@echo "  Target: linux/$(GOARCH)"
	@if [ -d "$(abspath $(PWD)/../../vmware/virtual-disks)" ]; then \
		echo "  virtual-disks: $(abspath $(PWD)/../../vmware/virtual-disks) (local)"; \
	else \
		echo "  ERROR: Local virtual-disks directory not found at $(abspath $(PWD)/../../vmware/virtual-disks)"; \
		exit 1; \
	fi
	docker run \
		--platform linux/$(GOARCH) \
		--rm \
		-e CGO_ENABLED=1 \
		-e GOEXPERIMENT=$(GOEXPERIMENT) \
		-e GOOS=linux \
		-e GOARCH=$(GOARCH) \
		-e GO111MODULE=on \
		-e GOFLAGS=-mod=readonly \
		-v $(abspath $(LIB_DIR)/vmware-vix-disklib-distrib):/usr/local/vmware-vix-disklib-distrib:ro \
		-v $(abspath $(PWD)/../..):/go/src/github.com:delegated \
		-v $(PWD)/.go/pkg:/go/pkg:delegated \
		-v $(PWD)/.go/cache:/root/.cache/go-build:delegated \
		-w /go/src/github.com/kubernetes-sigs/vsphere-csi-driver \
		$(DOCKER_IMAGE) \
		/bin/sh -c '\
			export CGO_CFLAGS="-I/usr/local/vmware-vix-disklib-distrib/include" && \
			export CGO_LDFLAGS="-L/usr/local/vmware-vix-disklib-distrib/lib64 -lvixDiskLib -ldl -Wl,-rpath,/usr/local/vmware-vix-disklib-distrib/lib64" && \
			go build -ldflags "-w -s -X \"sigs.k8s.io/vsphere-csi-driver/v3/pkg/csi/service.Version=v2.1.0-rc.1-2446-g86a0b120-dirty\"" -o $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH) cmd/$(CSI_BIN_NAME)/main.go'
	@echo "✓ Build complete: $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH)"
	@echo "✓ This binary has VDDK support and can be used for real CBT testing on Linux"

.PHONY: docker-build-syncer
docker-build-syncer:
	@echo "Building syncer in Docker container..."
	docker run \
		--platform linux/$(GOARCH) \
		--rm \
		-e CGO_ENABLED=0 \
		-e GOEXPERIMENT=$(GOEXPERIMENT) \
		-e GOOS=linux \
		-e GOARCH=$(GOARCH) \
		-e GO111MODULE=on \
		-e GOFLAGS=-mod=readonly \
		-v $(PWD):/workspace:delegated \
		-v $(PWD)/.go/pkg:/go/pkg:delegated \
		-v $(PWD)/.go/cache:/root/.cache/go-build:delegated \
		-w /workspace \
		$(DOCKER_IMAGE) \
		go build -ldflags "$(LDFLAGS_SYNCER)" -o $(BIN_OUT)/$(SYNCER_BIN_NAME).linux_$(GOARCH) cmd/$(SYNCER_BIN_NAME)/main.go
	@echo "✓ Build complete: $(BIN_OUT)/$(SYNCER_BIN_NAME).linux_$(GOARCH)"

.PHONY: docker-build-all
docker-build-all: docker-build-csi docker-build-syncer
	@echo "✓ All Docker builds complete"

# Verify the VDDK binary has CGO enabled
.PHONY: verify-vddk-binary
verify-vddk-binary:
	@echo "Verifying VDDK support in binary..."
	@if [ ! -f "$(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH)" ]; then \
		echo "ERROR: Binary not found. Run 'make docker-build-csi' first."; \
		exit 1; \
	fi
	@echo "Binary info:"
	@file $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH)
	@echo ""
	@echo "Build info:"
	@go version -m $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH) | grep -E "(CGO_ENABLED|GOARCH|GOOS)" || echo "  (build info not available)"
	@echo ""
	@if ldd $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH) 2>/dev/null | grep -q "libvixDiskLib"; then \
		echo "✓ Binary is dynamically linked with VDDK"; \
		ldd $(BIN_OUT)/$(CSI_BIN_NAME).linux_$(GOARCH) 2>/dev/null | grep vix || true; \
	else \
		echo "⚠ Binary appears to be statically linked or VDDK not detected"; \
		echo "  This is expected on non-Linux systems"; \
	fi

################################################################################
##                     DOCKER CONTAINER IMAGE BUILDS                          ##
################################################################################
# Docker image registry and tags
IMAGE_REGISTRY ?= localhost:5000
IMAGE_TAG ?= latest
CSI_IMAGE_NAME ?= vsphere-csi-driver
SYNCER_IMAGE_NAME ?= vsphere-syncer

# Version information
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

# Build container images with VDDK support
.PHONY: docker-image-csi-vddk
docker-image-csi-vddk:
ifeq ($(VDDK_FOR_DOCKER),no)
	@echo "ERROR: VDDK libraries not found at $(VDDK_LIBS)"
	@echo "Please install VDDK before building container images with VDDK support"
	@exit 1
endif
	@echo "Building CSI driver container image with VDDK support..."
	@echo "  Image: $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG)"
	@echo "  Version: $(BUILD_VERSION)"
	@echo "  Git Commit: $(GIT_COMMIT)"
	@echo "  Build context: $(abspath $(PWD)/../..)"
	docker build \
		--platform linux/$(GOARCH) \
		-f $(PWD)/images/driver/Dockerfile.vddk \
		-t $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG) \
		-t $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(BUILD_VERSION) \
		--build-arg VERSION=$(BUILD_VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg GOLANG_IMAGE=golang:1.24.2 \
		$(abspath $(PWD)/../..)
	@echo "✓ Container image built: $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG)"
	@echo "✓ This image includes VDDK libraries and has full CBT support"

# Build standard CSI driver container image (without VDDK)
.PHONY: docker-image-csi
docker-image-csi:
	@echo "Building CSI driver container image (without VDDK)..."
	@echo "  Image: $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG)"
	docker build \
		--platform linux/$(GOARCH) \
		-f images/driver/Dockerfile \
		-t $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG) \
		--build-arg VERSION=$(BUILD_VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		.
	@echo "✓ Container image built: $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG)"

# Build syncer container image
.PHONY: docker-image-syncer
docker-image-syncer:
	@echo "Building syncer container image..."
	@echo "  Image: $(IMAGE_REGISTRY)/$(SYNCER_IMAGE_NAME):$(IMAGE_TAG)"
	docker build \
		--platform linux/$(GOARCH) \
		-f images/syncer/Dockerfile \
		-t $(IMAGE_REGISTRY)/$(SYNCER_IMAGE_NAME):$(IMAGE_TAG) \
		--build-arg VERSION=$(BUILD_VERSION) \
		--build-arg GIT_COMMIT=$(GIT_COMMIT) \
		.
	@echo "✓ Container image built: $(IMAGE_REGISTRY)/$(SYNCER_IMAGE_NAME):$(IMAGE_TAG)"

# Build all container images with VDDK support
.PHONY: docker-images-vddk
docker-images-vddk: docker-image-csi-vddk docker-image-syncer
	@echo "✓ All container images with VDDK built successfully"

# Build all container images (without VDDK)
.PHONY: docker-images
docker-images: docker-image-csi docker-image-syncer
	@echo "✓ All container images built successfully"

# Push container images to registry
.PHONY: docker-push
docker-push:
	@echo "Pushing images to $(IMAGE_REGISTRY)..."
	docker push $(IMAGE_REGISTRY)/$(CSI_IMAGE_NAME):$(IMAGE_TAG)
	docker push $(IMAGE_REGISTRY)/$(SYNCER_IMAGE_NAME):$(IMAGE_TAG)
	@echo "✓ Images pushed successfully"

# List built images
.PHONY: docker-images-list
docker-images-list:
	@echo "Built images:"
	@docker images | grep -E "($(CSI_IMAGE_NAME)|$(SYNCER_IMAGE_NAME))" || echo "No images found"


# The Syncer binary.
SYNCER_BIN_NAME := syncer
SYNCER_BIN := $(BIN_OUT)/$(SYNCER_BIN_NAME).$(GOOS)_$(GOARCH)
build-syncer: $(SYNCER_BIN)
ifndef SYNCER_BIN_SRCS
SYNCER_BIN_SRCS := cmd/$(SYNCER_BIN_NAME)/main.go go.mod go.sum
SYNCER_BIN_SRCS += $(addsuffix /*.go,$(shell go list -f '{{ join .Deps "\n" }}' ./cmd/$(SYNCER_BIN_NAME) | grep $(MOD_NAME) | sed 's~$(MOD_NAME)~.~'))
export SYNCER_BIN_SRCS
endif

syncer_manifest: controller-gen
	$(CONTROLLER_GEN) crd paths=./pkg/apis/storagepool/... output:crd:dir=pkg/apis/storagepool/config
# find or download controller-gen
# download controller-gen if necessary
controller-gen:
ifeq (, $(shell which controller-gen))
	@{ \
	set -e ;\
	CONTROLLER_GEN_TMP_DIR=$$(mktemp -d) ;\
	cd $$CONTROLLER_GEN_TMP_DIR ;\
	go mod init tmp ;\
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 ;\
	rm -rf $$CONTROLLER_GEN_TMP_DIR ;\
	}
CONTROLLER_GEN=$(GOBIN)/controller-gen
else
CONTROLLER_GEN=$(shell which controller-gen)
endif

$(SYNCER_BIN): $(SYNCER_BIN_SRCS) syncer_manifest
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -ldflags '$(LDFLAGS_SYNCER)' -o $(abspath $@) $<
	@touch $@

# The default build target.
build build-bins: $(CSI_BIN) $(CSI_BIN_WINDOWS) $(SYNCER_BIN)
build-with-docker:
	hack/make.sh

################################################################################
##                                   DIST                                     ##
################################################################################
DIST_CSI_NAME := vsphere-csi-$(VERSION)
DIST_CSI_TGZ := $(DIST_OUT)/$(DIST_CSI_NAME)-$(GOOS)_$(GOARCH).tar.gz
dist-csi-tgz: build-dirs $(DIST_CSI_TGZ)
$(DIST_CSI_TGZ): $(CSI_BIN)
	_temp_dir=$$(mktemp -d) && cp $< "$${_temp_dir}/$(CSI_BIN_NAME)" && \
	tar czf $(abspath $@) README.md LICENSE -C "$${_temp_dir}" "$(CSI_BIN_NAME)" && \
	rm -fr "$${_temp_dir}"

DIST_CSI_ZIP := $(DIST_OUT)/$(DIST_CSI_NAME)-$(GOOS)_$(GOARCH).zip
dist-csi-zip: build-dirs $(DIST_CSI_ZIP)
$(DIST_CSI_ZIP): $(CSI_BIN)
	_temp_dir=$$(mktemp -d) && cp $< "$${_temp_dir}/$(CSI_BIN_NAME)" && \
	zip -j $(abspath $@) README.md LICENSE "$${_temp_dir}/$(CSI_BIN_NAME)" && \
	rm -fr "$${_temp_dir}"

dist-csi: dist-csi-tgz dist-csi-zip

DIST_SYNCER_NAME := vsphere-syncer-$(VERSION)
DIST_SYNCER_TGZ := $(BUILD_OUT)/dist/$(DIST_SYNCER_NAME)-$(GOOS)_$(GOARCH).tar.gz
dist-syncer-tgz: $(DIST_SYNCER_TGZ)
$(DIST_SYNCER_TGZ): $(SYNCER_BIN)
	_temp_dir=$$(mktemp -d) && cp $< "$${_temp_dir}/$(SYNCER_BIN_NAME)" && \
	tar czf $(abspath $@) README.md LICENSE -C "$${_temp_dir}" "$(SYNCER_BIN_NAME)" && \
	rm -fr "$${_temp_dir}"
DIST_SYNCER_ZIP := $(BUILD_OUT)/dist/$(DIST_SYNCER_NAME)-$(GOOS)_$(GOARCH).zip
dist-syncer-zip: $(DIST_SYNCER_ZIP)
$(DIST_SYNCER_ZIP): $(SYNCER_BIN)
	_temp_dir=$$(mktemp -d) && cp $< "$${_temp_dir}/$(SYNCER_BIN_NAME)" && \
	zip -j $(abspath $@) README.md LICENSE "$${_temp_dir}/$(SYNCER_BIN_NAME)" && \
	rm -fr "$${_temp_dir}"
dist-syncer: dist-syncer-tgz dist-syncer-zip

dist: dist-csi dist-syncer

################################################################################
##                                DEPLOY                                      ##
################################################################################
# The deploy target is for use by Prow.
.PHONY: deploy
deploy: | $(DOCKER_SOCK)
	$(MAKE) build-bins
	$(MAKE) unit-test
	$(MAKE) push-images

################################################################################
##                                 CLEAN                                      ##
################################################################################
.PHONY: clean
clean:
	@rm -f Dockerfile*
	rm -rf $(CSI_BIN) vsphere-csi-*.tar.gz vsphere-csi-*.zip \
		$(SYNCER_BIN) vsphere-syncer-*.tar.gz vsphere-syncer-*.zip \
		image-*.tar image-*.d $(DIST_OUT)/* $(BIN_OUT)/* .build/windows-driver.tar
	GO111MODULE=off go clean -i -x . ./cmd/$(CSI_BIN_NAME) ./cmd/$(SYNCER_BIN_NAME)

.PHONY: clean-d
clean-d:
	@find . -name "*.d" -type f -delete

.PHONY: clean-vddk
clean-vddk:
	@echo "Cleaning VDDK libraries..."
	rm -rf $(LIB_DIR)

################################################################################
##                                CROSS BUILD                                 ##
################################################################################

# Defining X_BUILD_DISABLED prevents the cross-build and cross-dist targets
# from being defined. This is to improve performance when invoking x-build
# or x-dist targets that invoke this Makefile. The nested call does not need
# to provide cross-build or cross-dist targets since it's the result of one.
ifndef X_BUILD_DISABLED

export X_BUILD_DISABLED := 1

# Modify this list to add new cross-build and cross-dist targets.
X_TARGETS ?= darwin_amd64 linux_amd64 linux_386 linux_arm linux_arm64 linux_ppc64le

X_TARGETS := $(filter-out $(GOOS)_$(GOARCH),$(X_TARGETS))

X_CSI_BINS := $(addprefix $(CSI_BIN_NAME).,$(X_TARGETS))
$(X_CSI_BINS):
	GOOS=$(word 1,$(subst _, ,$(subst $(CSI_BIN_NAME).,,$@))) GOARCH=$(word 2,$(subst _, ,$(subst $(CSI_BIN_NAME).,,$@))) $(MAKE) build-csi

x-build-csi: $(CSI_BIN) $(X_CSI_BINS)

x-build: x-build-csi

################################################################################
##                                CROSS DIST                                  ##
################################################################################

X_DIST_CSI_TARGETS := $(X_TARGETS)
X_DIST_CSI_TARGETS := $(addprefix $(DIST_CSI_NAME)-,$(X_DIST_CSI_TARGETS))
X_DIST_CSI_TGZS := $(addsuffix .tar.gz,$(X_DIST_CSI_TARGETS))
X_DIST_CSI_ZIPS := $(addsuffix .zip,$(X_DIST_CSI_TARGETS))
$(X_DIST_CSI_TGZS):
	GOOS=$(word 1,$(subst _, ,$(subst $(DIST_CSI_NAME)-,,$@))) GOARCH=$(word 2,$(subst _, ,$(subst $(DIST_CSI_NAME)-,,$(subst .tar.gz,,$@)))) $(MAKE) dist-csi-tgz
$(X_DIST_CSI_ZIPS):
	GOOS=$(word 1,$(subst _, ,$(subst $(DIST_CSI_NAME)-,,$@))) GOARCH=$(word 2,$(subst _, ,$(subst $(DIST_CSI_NAME)-,,$(subst .zip,,$@)))) $(MAKE) dist-csi-zip

x-dist-csi-tgzs: $(DIST_CSI_TGZ) $(X_DIST_CSI_TGZS)
x-dist-csi-zips: $(DIST_CSI_ZIP) $(X_DIST_CSI_ZIPS)
x-dist-csi: x-dist-csi-tgzs x-dist-csi-zips

x-dist: x-dist-csi

################################################################################
##                               CROSS CLEAN                                  ##
################################################################################

X_CLEAN_TARGETS := $(addprefix clean-,$(X_TARGETS))
.PHONY: $(X_CLEAN_TARGETS)
$(X_CLEAN_TARGETS):
	GOOS=$(word 1,$(subst _, ,$(subst clean-,,$@))) GOARCH=$(word 2,$(subst _, ,$(subst clean-,,$@))) $(MAKE) clean

.PHONY: x-clean
x-clean: clean $(X_CLEAN_TARGETS)

endif # ifndef X_BUILD_DISABLED

################################################################################
##                                 TESTING                                    ##
################################################################################
ifndef PKGS_WITH_TESTS
export PKGS_WITH_TESTS := $(sort $(shell find . -path ./tests -prune -o -name "*_test.go" -type f -exec dirname \{\} \;))
endif
TEST_FLAGS ?= -v -count=1

# Set up CGO flags for VDDK if available
ifeq ($(VDDK_AVAILABLE),yes)
export CGO_ENABLED := 1
export CGO_CFLAGS := -I$(abspath $(LIB_DIR)/vmware-vix-disklib-distrib)/include
export CGO_LDFLAGS := -L$(abspath $(VDDK_LIBS)) -lvixDiskLib -ldl -Wl,-rpath,$(abspath $(VDDK_LIBS))
VDDK_TEST_INFO := (with VDDK support)
else
export CGO_ENABLED := 0
VDDK_TEST_INFO := (without VDDK - CBT tests will be skipped)
endif

.PHONY: unit build-unit-tests
unit unit-test:
	@echo "Running unit tests $(VDDK_TEST_INFO)..."
	env -u VSPHERE_SERVER -u VSPHERE_DATACENTER -u VSPHERE_PASSWORD -u VSPHERE_USER -u VSPHERE_STORAGE_POLICY_NAME -u KUBECONFIG -u WCP_ENDPOINT -u WCP_PORT -u WCP_NAMESPACE -u TOKEN -u CERTIFICATE go test $(TEST_FLAGS) $(PKGS_WITH_TESTS)
unit-cover:
	env -u VSPHERE_SERVER -u VSPHERE_DATACENTER -u VSPHERE_PASSWORD -u VSPHERE_USER -u VSPHERE_STORAGE_POLICY_NAME -u KUBECONFIG -u WCP_ENDPOINT -u WCP_PORT -u WCP_NAMESPACE -u TOKEN -u CERTIFICATE go test $(TEST_FLAGS) $(PKGS_WITH_TESTS) && go tool cover -html=cover.out
build-unit-tests:
	$(foreach pkg,$(PKGS_WITH_TESTS),go test $(TEST_FLAGS) -c $(pkg); )

INTEGRATION_TEST_PKGS ?=
.PHONY: integration-unit-test
integration-unit-test:
ifndef TYPE
	$(error Requires TYPE from a deployed testbed to run integration-unit-test)
else
    ifeq ($(TYPE), guestcluster)
        ifndef WCP_ENDPOINT
            $(error Requires WCP_ENDPOINT from a deployed testbed to run integration-unit-test)
        endif
        ifndef WCP_NAMESPACE
            $(error Requires WCP_NAMESPACE from a deployed testbed to run integration-unit-test)
        endif
        ifndef SUPERVISOR_STORAGE_CLASS
            $(error Requires SUPERVISOR_STORAGE_CLASS from a deployed testbed to run integration-unit-test)
        endif
        ifndef TOKEN
            $(error Requires TOKEN from a deployed testbed to run integration-unit-test)
        endif
        ifndef CERTIFICATE
            $(error Requires CERTIFICATE from a deployed testbed to run integration-unit-test)
        else
	        $(eval INTEGRATION_TEST_PKGS += ./pkg/csi/service/wcpguest)
        endif
    else
        ifndef VSPHERE_VCENTER
            $(error Requires VSPHERE_VCENTER from a deployed testbed to run integration-unit-test)
        endif
        ifndef VSPHERE_USER
            $(error Requires VSPHERE_USER from a deployed testbed to run integration-unit-test)
        endif
        ifndef VSPHERE_PASSWORD
            $(error Requires VSPHERE_PASSWORD from a deployed testbed to run integration-unit-test)
        endif
        ifndef VSPHERE_DATACENTER
            $(error Requires VSPHERE_DATACENTER from a deployed testbed to run integration-unit-test)
        endif
        ifndef VSPHERE_DATASTORE_URL
            $(error Requires VSPHERE_DATASTORE_URL from a deployed testbed to run integration-unit-test)
        endif
        ifndef VSPHERE_INSECURE
            $(error Requires VSPHERE_INSECURE from a deployed testbed to run integration-unit-test)
        endif
        ifeq ($(TYPE), supervisorcluster)
	        $(eval INTEGRATION_TEST_PKGS += ./pkg/csi/service/wcp ./pkg/syncer)
        else
            ifndef VSPHERE_K8S_NODE
                $(error Requires VSPHERE_K8S_NODE from a deployed testbed to run integration-unit-test)
            endif
            ifndef KUBECONFIG
                $(error Requires KUBECONFIG from a deployed testbed to run integration-unit-test)
            else
		$(eval INTEGRATION_TEST_PKGS += ./pkg/csi/service/vanilla ./pkg/syncer ./pkg/common/utils)
            endif
        endif
    endif
endif
	go test $(TEST_FLAGS) -tags=integration-unit $(INTEGRATION_TEST_PKGS)

# The default test target.
.PHONY: test test-cover build-tests
test: unit
test-cover: unit-cover
build-tests: build-unit-tests

.PHONY: cover
cover: TEST_FLAGS += -cover
cover: test

.PHONY: coverprofile
coverprofile: TEST_FLAGS += -coverprofile cover.out
coverprofile: test-cover

.PHONY: test-e2e
test-e2e:
	hack/run-e2e-test.sh

# Test Changed Block Tracking (requires VDDK)
.PHONY: test-cbt
test-cbt: check-vddk
ifeq ($(VDDK_AVAILABLE),yes)
	@echo "Running Changed Block Tracking tests with VDDK..."
	CGO_ENABLED=1 go test $(TEST_FLAGS) -run TestWCPGetMetadata ./pkg/csi/service/wcp
else
	$(error VDDK is required for CBT tests. Please install VDDK to $(LIB_DIR)/vmware-vix-disklib-distrib/)
endif
################################################################################
##                                 LINTING                                    ##
################################################################################
.PHONY: check fmt mdlint shellcheck vet
check: fmt mdlint shellcheck staticcheck vet golangci-lint

fmt:
	hack/check-format.sh

mdlint:
	hack/check-mdlint.sh

golangci-lint:
	hack/check-golangci-lint.sh

shellcheck:
	hack/check-shell.sh

staticcheck:
	hack/check-staticcheck.sh

vet:
	hack/check-vet.sh

################################################################################
##                                 BUILD IMAGES                               ##
################################################################################
.PHONY: images
images: | $(DOCKER_SOCK)
	hack/release.sh

################################################################################
##                                  PUSH IMAGES                               ##
################################################################################
.PHONY: push-images upload-images
push-images: | $(DOCKER_SOCK)
ifndef CSI_REGISTRY
	hack/release.sh -p
else 
	hack/release.sh -p -r ${CSI_REGISTRY}
endif

################################################################################
##                                  CI IMAGE                                  ##
################################################################################
build-ci-image:
	$(MAKE) -C images/ci build

push-ci-image:
	$(MAKE) -C images/ci push

print-ci-image:
	@$(MAKE) --no-print-directory -C images/ci print
