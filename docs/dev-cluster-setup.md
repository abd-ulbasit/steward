# Dev Cluster Setup Guide

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| kind | >= 0.20 | `brew install kind` or [releases](https://kind.sigs.k8s.io/) |
| kubectl | >= 1.28 | `brew install kubectl` |
| docker | >= 24.0 | [Docker Desktop](https://www.docker.com/products/docker-desktop/) or Colima |
| make | any | Pre-installed on macOS/Linux |

## Quick Start

```bash
# 1. Create cluster with CNPG operator + deploy controller
./hack/setup-dev-cluster.sh

# 2. Apply a sample Application
kubectl apply -f config/samples/platform_v1alpha1_application.yaml

# 3. Watch it provision
kubectl get applications -w

# 4. Run full lifecycle validation
./hack/validate-e2e.sh
```

## What Gets Installed

| Component | Version | Namespace | Purpose |
|-----------|---------|-----------|---------|
| Kind cluster | latest | n/a | Local Kubernetes |
| CNPG operator | v1.25.0 | cnpg-system | PostgreSQL provisioning |
| Steward CRDs | latest | n/a | Application CRD |
| Steward controller | dev build | steward-system | Reconciliation |

## Script Flags

```bash
./hack/setup-dev-cluster.sh                # Full setup (create cluster, install CNPG, build + deploy)
./hack/setup-dev-cluster.sh --skip-build   # Skip docker build (use existing image)
./hack/setup-dev-cluster.sh --skip-operators  # Skip CNPG operator installation
./hack/setup-dev-cluster.sh --teardown     # Delete the Kind cluster
```

## Daily Workflow

After making code changes:

```bash
# Rebuild and redeploy (skip operator reinstall)
./hack/setup-dev-cluster.sh --skip-operators

# Or manually: rebuild image, load into Kind, restart controller
make docker-build IMG=steward:dev
kind load docker-image steward:dev --name steward-dev
kubectl rollout restart deployment/steward-controller-manager -n steward-system
```

## Resource Naming Conventions

The KubernetesProvider creates child resources following this pattern:

| Resource Type | Name Pattern | Example |
|---------------|-------------|---------|
| CNPG Cluster | `{app}-db` | `application-sample-db` |
| DB Secret | `{app}-db-credentials` | `application-sample-db-credentials` |
| Redis Failover | `{app}-cache` | `application-sample-cache` |
| Cache Secret | `{app}-cache-credentials` | `application-sample-cache-credentials` |
| RabbitMQ Cluster | `{app}-queue` | `application-sample-queue` |
| Queue Secret | `{app}-queue-credentials` | `application-sample-queue-credentials` |

## Troubleshooting

| Problem | Diagnosis | Fix |
|---------|-----------|-----|
| CNPG operator not ready | `kubectl get pods -n cnpg-system` | Wait or reinstall: `./hack/setup-dev-cluster.sh` |
| Controller 403 Forbidden | `kubectl logs -n steward-system deploy/steward-controller-manager` | Run `make manifests && make deploy IMG=steward:dev` |
| CNPG Cluster stuck | `kubectl describe cluster <name>` | Check CNPG logs: `kubectl logs -n cnpg-system deploy/cnpg-controller-manager` |
| Image not found in Kind | `kind load docker-image` failed | Ensure docker has the image: `docker images \| grep steward` |
| Application stuck Provisioning | `kubectl describe application <name>` | Check conditions for specific component failure |

## Adding More Operators

To add Redis (Spotahome) or RabbitMQ operators later:

1. Add install commands to `hack/setup-dev-cluster.sh`
2. Update sample Application in `config/samples/`
3. Add validation checks to `hack/validate-e2e.sh`
4. Update this document
