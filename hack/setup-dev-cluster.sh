#!/usr/bin/env bash
# =============================================================================
# setup-dev-cluster.sh — Create a Kind cluster with CNPG for GoPlatform dev
# =============================================================================
#
# Usage:
#   ./hack/setup-dev-cluster.sh              # Full setup
#   ./hack/setup-dev-cluster.sh --skip-build # Skip docker build
#   ./hack/setup-dev-cluster.sh --teardown   # Delete the cluster
#
# Prerequisites: kind, kubectl, docker, make
# =============================================================================

set -euo pipefail

CLUSTER_NAME="goplatform-dev"
CNPG_VERSION="1.25.0"
CERT_MANAGER_VERSION="v1.17.2"
PROM_OPERATOR_CRD_VERSION="v0.89.0"
IMG="goplatform:dev"
SKIP_BUILD=false
SKIP_OPERATORS=false
TEARDOWN=false

# Parse flags
for arg in "$@"; do
  case $arg in
    --skip-build) SKIP_BUILD=true ;;
    --skip-operators) SKIP_OPERATORS=true ;;
    --teardown) TEARDOWN=true ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# ── Teardown ──────────────────────────────────────────────────────────────────
if [ "$TEARDOWN" = true ]; then
  info "Deleting Kind cluster: $CLUSTER_NAME"
  kind delete cluster --name "$CLUSTER_NAME" 2>/dev/null || true
  info "Done."
  exit 0
fi

# ── Prerequisites ─────────────────────────────────────────────────────────────
for cmd in kind kubectl docker make; do
  command -v "$cmd" >/dev/null 2>&1 || error "$cmd is required but not installed."
done
info "All prerequisites found."

# ── Step 1: Create Kind cluster ──────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  info "Kind cluster '$CLUSTER_NAME' already exists, skipping creation."
else
  info "Creating Kind cluster: $CLUSTER_NAME"
  kind create cluster --name "$CLUSTER_NAME" --wait 60s
fi

# Ensure kubectl context is set
kubectl cluster-info --context "kind-${CLUSTER_NAME}" >/dev/null 2>&1 || \
  error "Cannot connect to cluster. Check 'kind-${CLUSTER_NAME}' context."
info "Cluster is ready."

# ── Step 2: Install cert-manager ─────────────────────────────────────────────
if [ "$SKIP_OPERATORS" = false ]; then
  if kubectl get namespace cert-manager --context "kind-${CLUSTER_NAME}" >/dev/null 2>&1; then
    info "cert-manager already installed, skipping."
  else
    info "Installing cert-manager ${CERT_MANAGER_VERSION}..."
    kubectl apply --context "kind-${CLUSTER_NAME}" \
      -f "https://github.com/cert-manager/cert-manager/releases/download/${CERT_MANAGER_VERSION}/cert-manager.yaml"

    info "Waiting for cert-manager webhook to be ready..."
    kubectl wait --for=condition=Available deployment/cert-manager-webhook \
      -n cert-manager --context "kind-${CLUSTER_NAME}" --timeout=120s
    info "cert-manager is ready."
  fi
else
  info "Skipping cert-manager installation (--skip-operators)."
fi

# ── Step 3: Install Prometheus Operator CRDs ─────────────────────────────────
if [ "$SKIP_OPERATORS" = false ]; then
  if kubectl get crd servicemonitors.monitoring.coreos.com --context "kind-${CLUSTER_NAME}" >/dev/null 2>&1; then
    info "Prometheus Operator CRDs already installed, skipping."
  else
    info "Installing Prometheus Operator CRDs ${PROM_OPERATOR_CRD_VERSION}..."
    kubectl apply --server-side --context "kind-${CLUSTER_NAME}" \
      -f "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${PROM_OPERATOR_CRD_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml" \
      -f "https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/${PROM_OPERATOR_CRD_VERSION}/example/prometheus-operator-crd/monitoring.coreos.com_prometheusrules.yaml"
    info "Prometheus Operator CRDs installed."
  fi
else
  info "Skipping Prometheus CRDs installation (--skip-operators)."
fi

# ── Step 4: Install CNPG operator ────────────────────────────────────────────
if [ "$SKIP_OPERATORS" = false ]; then
  if kubectl get crd clusters.postgresql.cnpg.io --context "kind-${CLUSTER_NAME}" >/dev/null 2>&1; then
    info "CNPG operator already installed, skipping."
  else
    info "Installing CNPG operator v${CNPG_VERSION}..."
    kubectl apply --server-side --context "kind-${CLUSTER_NAME}" \
      -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-${CNPG_VERSION}.yaml"

    info "Waiting for CNPG operator to be ready..."
    kubectl wait --for=condition=Available deployment/cnpg-controller-manager \
      -n cnpg-system --context "kind-${CLUSTER_NAME}" --timeout=120s
    info "CNPG operator is ready."
  fi
else
  info "Skipping operator installation (--skip-operators)."
fi

# ── Step 5: Install GoPlatform CRDs ─────────────────────────────────────────
info "Installing GoPlatform CRDs..."
make install
info "CRDs installed."

# ── Step 6: Build and load controller image ──────────────────────────────────
if [ "$SKIP_BUILD" = false ]; then
  info "Building controller image: $IMG"
  make docker-build IMG="$IMG"

  info "Loading image into Kind cluster..."
  kind load docker-image "$IMG" --name "$CLUSTER_NAME"
  info "Image loaded."
else
  info "Skipping build (--skip-build)."
fi

# ── Step 7: Deploy controller ────────────────────────────────────────────────
info "Deploying GoPlatform controller..."
make deploy IMG="$IMG"

info "Waiting for controller to be ready..."
kubectl wait --for=condition=Available deployment/goplatform-controller-manager \
  -n goplatform-system --context "kind-${CLUSTER_NAME}" --timeout=120s
info "Controller is ready."

# ── Done ─────────────────────────────────────────────────────────────────────
echo ""
info "============================================"
info " Dev cluster is ready!"
info "============================================"
info ""
info " Cluster:    kind-${CLUSTER_NAME}"
info " CNPG:       v${CNPG_VERSION}"
info " Controller: deployed in goplatform-system"
info ""
info " Next steps:"
info "   kubectl apply -f config/samples/platform_v1alpha1_application.yaml"
info "   kubectl get applications"
info "   kubectl get clusters.postgresql.cnpg.io"
info ""
info " To validate end-to-end:"
info "   ./hack/validate-e2e.sh"
info ""
info " To teardown:"
info "   ./hack/setup-dev-cluster.sh --teardown"
