#!/bin/bash
set -euo pipefail

echo "Installing Kubebuilder development tools..."

ARCH=$(go env GOARCH)

# Tool versions and their SHA-256 checksums, taken from each project's
# published checksum files. Pinning means a moved "latest" or a tampered
# download fails the install instead of landing in /usr/local/bin.
KIND_VERSION=v0.33.0
KUBEBUILDER_VERSION=v4.16.0
KUBECTL_VERSION=v1.35.9

case "${ARCH}" in
  amd64)
    KIND_SHA256=aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d
    KUBEBUILDER_SHA256=539984e58832044f2326b1bb940c1eaf784f1849e65d06d3a4a4a8b5dfac7ca7
    KUBECTL_SHA256=3cfeaf80be482b435b0aa214aff6e0b2c312ee23c0ff20810c75517b6004c6eb
    ;;
  arm64)
    KIND_SHA256=20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2
    KUBEBUILDER_SHA256=6833432c8faab62d2ca6b583f32c88b0a3d1b7264562739c202084c996a55845
    KUBECTL_SHA256=39c98bca82875d9a9ddfb6f3c5ab17c3f78ea0801230683e538a67d5c053308d
    ;;
  *)
    echo "No pinned tool checksums for ${ARCH}" >&2
    exit 1
    ;;
esac

# install_verified URL SHA256 NAME downloads URL, checks it, installs as NAME.
install_verified() {
  local url=$1 sum=$2 name=$3
  curl -fsSL -o "./${name}" "${url}"
  echo "${sum}  ./${name}" | sha256sum --check --strict
  chmod +x "./${name}"
  mv "./${name}" "/usr/local/bin/${name}"
}

if ! command -v kind &> /dev/null; then
  install_verified "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-${ARCH}" "${KIND_SHA256}" kind
fi

if ! command -v kubebuilder &> /dev/null; then
  install_verified \
    "https://github.com/kubernetes-sigs/kubebuilder/releases/download/${KUBEBUILDER_VERSION}/kubebuilder_linux_${ARCH}" \
    "${KUBEBUILDER_SHA256}" kubebuilder
fi

if ! command -v kubectl &> /dev/null; then
  install_verified "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" "${KUBECTL_SHA256}" kubectl
fi

# Wait for Docker to be ready
for i in {1..30}; do
  if docker info >/dev/null 2>&1; then
    break
  fi
  if [ $i -eq 30 ]; then
    echo "WARNING: Docker not ready after 30s"
  fi
  sleep 1
done

# Create kind network, ignore errors if exists or conflicts
docker network inspect kind >/dev/null 2>&1 || docker network create kind || true

# Verify installations
echo "Installed versions:"
kind version
kubebuilder version
kubectl version --client
docker --version
go version

echo "DevContainer ready!"
