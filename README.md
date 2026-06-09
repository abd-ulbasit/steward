# GoPlatform

**Kubernetes Operator for Self-Service Infrastructure Provisioning**

A learning-focused Kubernetes operator built with kubebuilder v4 that transforms declarative Application CRDs into fully provisioned, in-cluster infrastructure. Developers declare what they need; the operator provisions databases, caches, queues, and monitoring automatically.

---

## The Problem

Platform teams spend endless hours on:
- **Ticket-based provisioning** - "Please create my database" (3-day SLA)
- **Snowflake infrastructure** - Every service configured differently
- **Knowledge silos** - Only platform team knows how to deploy

## The Solution

Developers declare what they need. The platform provisions everything automatically:

```yaml
apiVersion: platform.goplatform.io/v1alpha1
kind: Application
metadata:
  name: payments-api
spec:
  team: payments
  tier: critical

  workload:
    image: ghcr.io/org/payments-api:v1.0.0
    replicas: 3

  database:
    type: postgres
    size: small
    highAvailability: true

  cache:
    type: redis
    size: small

  queue:
    type: rabbitmq
    deadLetterQueue:
      enabled: true

# GoPlatform automatically provisions:
# ✅ Kubernetes Deployment, Service, HPA, PDB
# ✅ CloudNativePG PostgreSQL cluster
# ✅ Redis via Spotahome operator
# ✅ RabbitMQ via Cluster Operator
# ✅ Credential Secrets with connection strings
# ✅ Prometheus ServiceMonitor + AlertRules
```

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Developer Interface                            │
│                              kubectl apply                                  │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                         Kubernetes API Server                               │
│                         Application CRD (v1alpha1)                          │
└─────────────────────────────────────────────────────────────────────────────┘
                                       │ watch
                                       ▼
┌─────────────────────────────────────────────────────────────────────────────┐
│                        GoPlatform Controller                                │
│  ┌─────────────────┐  ┌──────────────────┐  ┌────────────────────────────┐  │
│  │ App Reconciler  │  │ Infra Provider   │  │ Admission Webhooks         │  │
│  │ - K8s resources │  │ - CNPG           │  │ - Validating               │  │
│  │ - Status mgmt   │  │ - Redis          │  │ - Mutating                 │  │
│  │ - Conditions    │  │ - RabbitMQ       │  │ - Conversion               │  │
│  └─────────────────┘  └──────────────────┘  └────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────────────────┘
          │                      │                         │
          ▼                      ▼                         ▼
┌─────────────────┐   ┌─────────────────────┐   ┌─────────────────────────┐
│ K8s Resources   │   │ Operator CRDs       │   │ Monitoring              │
│ - Deployment    │   │ - CNPG Cluster      │   │ - ServiceMonitor        │
│ - Service       │   │ - RedisFailover     │   │ - PrometheusRule        │
│ - HPA, PDB      │   │ - RabbitmqCluster   │   │ - Controller metrics    │
│ - ConfigMap     │   │ - PVCs              │   │                         │
│ - Secret        │   │ - Secrets           │   │                         │
└─────────────────┘   └─────────────────────┘   └─────────────────────────┘
```

---

## Quick Start

### Prerequisites
- Kubernetes 1.28+ cluster (Kind, Colima, or managed)
- kubectl configured

### Installation
```bash
# Install CRDs
make install

# Run controller locally
make run
```

### Deploy an Application
```bash
kubectl apply -f config/samples/platform_v1alpha1_application.yaml

# Watch provisioning status
kubectl get applications -w

# Check detailed status
kubectl describe application demo-app
```

---

## Development Phases

| Phase | Description | Milestones | Status |
|-------|-------------|------------|--------|
| **Phase 1** | Solid Foundation (CRDs, Controller, Status, Finalizers, Provider) | M1-M5 | ✅ Complete |
| **Phase 2** | Real-World Operator (Integration, Webhooks, Observability, Drift) | M6-M9 | 🔄 In Progress |
| **Phase 3** | Advanced Patterns (Multi-version CRD, Kyverno, E2E/CI) | M10-M12 | 📋 Planned |

See [PROGRESS.md](PROGRESS.md) for detailed milestone tracking with learning objectives and implementation guides.

---

## Tech Stack

- **Language:** Go 1.22+
- **Operator Framework:** controller-runtime, kubebuilder v4
- **Infrastructure:** CloudNativePG, Spotahome Redis Operator, RabbitMQ Cluster Operator
- **Monitoring:** Prometheus, Grafana (via Prometheus Operator CRDs)
- **Policy:** Kyverno
- **Testing:** Ginkgo/Gomega, envtest, Kind

---

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for development setup and guidelines.

---

## License

MIT License - see [LICENSE](LICENSE) for details.
