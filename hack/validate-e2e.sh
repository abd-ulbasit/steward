#!/usr/bin/env bash
# =============================================================================
# validate-e2e.sh — End-to-end lifecycle validation for Steward
# =============================================================================
#
# Prerequisites: Kind cluster created by hack/setup-dev-cluster.sh
#
# Validates:
#   1. Create Application → CNPG Cluster CR appears
#   2. Application reaches phase=Ready
#   3. Status conditions and infrastructure fields populated
#   4. Delete Application → all resources cleaned up
#
# Usage:
#   ./hack/validate-e2e.sh              # Full validation (create + verify + delete + cleanup)
#   ./hack/validate-e2e.sh --no-cleanup # Create + verify only; leave resources for inspection
# =============================================================================

set -euo pipefail

CLUSTER_NAME="steward-dev"
CONTEXT="kind-${CLUSTER_NAME}"
SAMPLE="config/samples/platform_v1alpha1_application.yaml"
APP_NAME="application-sample"
NAMESPACE="default"
PASS=0
FAIL=0
NO_CLEANUP=false

# Parse flags
for arg in "$@"; do
  case $arg in
    --no-cleanup) NO_CLEANUP=true ;;
    *) echo "Unknown flag: $arg"; exit 1 ;;
  esac
done

# Naming conventions from KubernetesProvider:
#   CNPG Cluster:  {app.Name}-db
#   Secret:        {app.Name}-db-credentials
CNPG_CLUSTER_NAME="${APP_NAME}-db"
SECRET_NAME="${APP_NAME}-db-credentials"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "  ${GREEN}PASS${NC} $*"; PASS=$((PASS + 1)); }
fail() { echo -e "  ${RED}FAIL${NC} $*"; FAIL=$((FAIL + 1)); }
info() { echo -e "${YELLOW}>>>${NC} $*"; }

# ── Verify cluster connection ────────────────────────────────────────────────
kubectl cluster-info --context "$CONTEXT" >/dev/null 2>&1 || {
  echo -e "${RED}Cannot connect to cluster '$CONTEXT'. Run hack/setup-dev-cluster.sh first.${NC}"
  exit 1
}

# ── Clean up any previous run ────────────────────────────────────────────────
info "Cleaning up previous test resources..."
kubectl delete application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  --ignore-not-found --timeout=60s 2>/dev/null || true
sleep 2

# ── Step 1: Apply sample Application ────────────────────────────────────────
info "Step 1: Applying sample Application..."
if kubectl apply -f "$SAMPLE" --context "$CONTEXT"; then
  pass "Application created"
else
  fail "Failed to create Application"
  exit 1
fi

# ── Step 2: Wait for CNPG Cluster CR ────────────────────────────────────────
info "Step 2: Waiting for CNPG Cluster CR '${CNPG_CLUSTER_NAME}' to appear (60s timeout)..."
CNPG_FOUND=false
for i in $(seq 1 12); do
  if kubectl get clusters.postgresql.cnpg.io "$CNPG_CLUSTER_NAME" -n "$NAMESPACE" \
      --context "$CONTEXT" >/dev/null 2>&1; then
    CNPG_FOUND=true
    break
  fi
  sleep 5
done

if [ "$CNPG_FOUND" = true ]; then
  pass "CNPG Cluster CR '${CNPG_CLUSTER_NAME}' created"
else
  fail "CNPG Cluster CR '${CNPG_CLUSTER_NAME}' not found after 60s"
  info "Debug: kubectl get clusters.postgresql.cnpg.io -n $NAMESPACE --context $CONTEXT"
  info "Debug: kubectl logs -n steward-system deploy/steward-controller-manager --context $CONTEXT --tail=50"
fi

# ── Step 3: Wait for Application phase=Ready ────────────────────────────────
info "Step 3: Waiting for Application phase=Ready (5m timeout)..."
if kubectl wait --for=jsonpath='{.status.phase}'=Ready \
  "application/${APP_NAME}" \
  -n "$NAMESPACE" --context "$CONTEXT" --timeout=300s 2>/dev/null; then
  pass "Application reached phase=Ready"
else
  PHASE=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
    -o jsonpath='{.status.phase}' 2>/dev/null || echo "unknown")
  fail "Application phase is '$PHASE' (expected Ready)"
  info "Debug: kubectl describe application $APP_NAME -n $NAMESPACE --context $CONTEXT"
fi

# ── Step 4: Verify conditions ───────────────────────────────────────────────
info "Step 4: Verifying status conditions..."

DB_READY=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  -o jsonpath='{.status.conditions[?(@.type=="DatabaseReady")].status}' 2>/dev/null || echo "")
if [ "$DB_READY" = "True" ]; then
  pass "DatabaseReady=True"
else
  fail "DatabaseReady='$DB_READY' (expected True)"
fi

READY=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
if [ "$READY" = "True" ]; then
  pass "Ready=True"
else
  fail "Ready='$READY' (expected True)"
fi

# ── Step 5: Verify credential Secret ────────────────────────────────────────
info "Step 5: Verifying credential Secret '${SECRET_NAME}'..."
if kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" --context "$CONTEXT" >/dev/null 2>&1; then
  pass "Credential Secret '${SECRET_NAME}' exists"
else
  fail "Credential Secret '${SECRET_NAME}' not found"
fi

# ── Step 6: Verify status.infrastructure ────────────────────────────────────
info "Step 6: Verifying status.infrastructure fields..."

DB_ENDPOINT=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  -o jsonpath='{.status.infrastructure.database.endpoint}' 2>/dev/null || echo "")
if [ -n "$DB_ENDPOINT" ]; then
  pass "Database endpoint populated: ${DB_ENDPOINT}"
else
  fail "Database endpoint is empty"
fi

DB_PORT=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  -o jsonpath='{.status.infrastructure.database.port}' 2>/dev/null || echo "")
if [ "$DB_PORT" = "5432" ]; then
  pass "Database port=5432"
else
  fail "Database port='$DB_PORT' (expected 5432)"
fi

DB_SECRET=$(kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" \
  -o jsonpath='{.status.infrastructure.database.secretRef.name}' 2>/dev/null || echo "")
if [ "$DB_SECRET" = "$SECRET_NAME" ]; then
  pass "Database secretRef='${SECRET_NAME}'"
else
  fail "Database secretRef='$DB_SECRET' (expected '${SECRET_NAME}')"
fi

if [ "$NO_CLEANUP" = true ]; then
  # ── No-cleanup mode: skip deletion, print useful inspection commands ──────
  info "Skipping cleanup (--no-cleanup). Resources left in cluster for inspection."
  echo ""
  info "Useful commands:"
  info "  kubectl get application $APP_NAME -n $NAMESPACE --context $CONTEXT -o yaml"
  info "  kubectl describe application $APP_NAME -n $NAMESPACE --context $CONTEXT"
  info "  kubectl get clusters.postgresql.cnpg.io $CNPG_CLUSTER_NAME -n $NAMESPACE --context $CONTEXT -o yaml"
  info "  kubectl get secret $SECRET_NAME -n $NAMESPACE --context $CONTEXT -o jsonpath='{.data}'"
  info "  kubectl logs -n steward-system deploy/steward-controller-manager --context $CONTEXT --tail=50"
  echo ""
  info "To clean up manually:"
  info "  kubectl delete application $APP_NAME -n $NAMESPACE --context $CONTEXT"
else
  # ── Step 7: Delete Application ──────────────────────────────────────────────
  info "Step 7: Deleting Application..."
  kubectl delete application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" --timeout=120s
  pass "Application delete command succeeded"

  # ── Step 8: Verify cleanup ──────────────────────────────────────────────────
  info "Step 8: Verifying cleanup (2m timeout)..."

  # Wait for CNPG Cluster to be deleted
  CLEANUP_OK=true
  for i in $(seq 1 24); do
    if ! kubectl get clusters.postgresql.cnpg.io "$CNPG_CLUSTER_NAME" -n "$NAMESPACE" \
        --context "$CONTEXT" >/dev/null 2>&1; then
      break
    fi
    if [ "$i" -eq 24 ]; then
      CLEANUP_OK=false
    fi
    sleep 5
  done

  if [ "$CLEANUP_OK" = true ]; then
    pass "CNPG Cluster cleaned up"
  else
    fail "CNPG Cluster still exists after 2m"
  fi

  # Check Secret is gone
  if ! kubectl get secret "$SECRET_NAME" -n "$NAMESPACE" --context "$CONTEXT" >/dev/null 2>&1; then
    pass "Credential Secret cleaned up"
  else
    fail "Credential Secret still exists"
  fi

  # Check Application is gone
  if ! kubectl get application "$APP_NAME" -n "$NAMESPACE" --context "$CONTEXT" >/dev/null 2>&1; then
    pass "Application fully deleted"
  else
    fail "Application still exists (finalizer stuck?)"
  fi
fi

# ── Summary ─────────────────────────────────────────────────────────────────
echo ""
echo "============================================"
echo -e " Results: ${GREEN}${PASS} passed${NC}, ${RED}${FAIL} failed${NC}"
echo "============================================"

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
