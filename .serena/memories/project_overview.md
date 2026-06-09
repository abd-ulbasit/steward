# GoPlatform Overview

## Purpose
GoPlatform is an Internal Developer Platform (IDP) implemented as a Kubernetes operator. It turns declarative Application CRDs into fully provisioned, observable infrastructure (K8s resources + cloud resources via Terraform).

## Tech Stack
- Language: Go (go.mod targets Go 1.25.3)
- Operator framework: controller-runtime + kubebuilder
- Kubernetes APIs: k8s.io/apimachinery, k8s.io/client-go, k8s.io/api
- Testing: Ginkgo/Gomega with envtest
- IaC: Terraform (planned integration)

## Project Structure (high level)
- cmd/                 Entry point (manager: cmd/main.go)
- api/                 CRD types and schema markers
- internal/controller/ Reconciliation logic, metrics, monitoring
- internal/provider/   InfrastructureProvider interface + impls
- internal/webhook/    Admission webhook validation
- config/              CRD/RBAC/manifests (generated)
- examples/            Sample Application manifests
- docs/                Architecture & observability docs

## Entry Points
- Operator binary: cmd/main.go
- Local dev run: `make run`

## Key Patterns
- Reconciliation loop (level-triggered)
- Finalizers for safe deletion
- Status conditions for observability
- CreateOrUpdate for child resources

## Notes
- Auto-generated files must not be edited (CRDs, RBAC, zz_generated).
- Use kubebuilder markers in *_types.go for schema updates.
