# Contributing to Steward

Thank you for considering contributing to Steward! This document outlines the process for contributing to the project.

## Development Setup

### Prerequisites
- Go (version pinned in `go.mod`)
- Docker
- `kubectl`
- `kind`

Everything else — controller-gen, kustomize, golangci-lint, envtest binaries — is
downloaded into `bin/` by the Makefile on first use.

### Getting Started

```bash
git clone https://github.com/abd-ulbasit/steward.git
cd steward
go mod download

# Kind cluster + cert-manager + Prometheus Operator CRDs + CNPG + the Application CRD
make dev-setup

# Run the controller locally against that cluster
make run
```

`make dev-teardown` deletes the cluster. To install only the CRD into a cluster
you already have, use `make install`.

## Project Structure

```
steward/
├── api/v1alpha1/        # CRD types & schema markers
├── cmd/                 # Entry point (manager: cmd/main.go)
├── internal/
│   ├── controller/      # Reconciliation logic, metrics, monitoring
│   ├── provider/        # InfrastructureProvider interface + impls
│   └── webhook/         # Admission webhook validation
├── config/              # Generated CRD/RBAC/manifests (do not hand-edit)
├── examples/            # Sample Application manifests
├── hack/                # Dev scripts
├── test/                # E2E + test utils
└── docs/                # Documentation
```

## Making Changes

### Before You Start
1. Check existing issues for related work
2. Open an issue for significant changes
3. Discuss approach before implementation

### Development Process
1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Make your changes with comprehensive comments
4. Add tests for new functionality
5. Run tests: `make test`
6. Commit with clear messages: `git commit -m 'Add feature X'`
7. Push to your fork: `git push origin feature/my-feature`
8. Open a Pull Request

### Code Standards

#### Go Code
- Follow idiomatic Go patterns
- Add comprehensive comments explaining WHY and HOW
- Include ASCII diagrams for complex flows
- Handle all errors explicitly
- Write tests for new functionality

#### Comment Standards
Every significant function should have comments explaining:
- **WHY** - Why does this pattern/function exist?
- **HOW** - How does it work internally?
- **ALTERNATIVES** - What other approaches were considered?
- **TRADEOFFS** - What are the pros/cons?

Example:
```go
// ============================================================================
// RECONCILIATION LOOP
// ============================================================================
//
// WHY: The controller needs to continuously ensure actual state matches
// desired state. This is the "reconciliation" pattern.
//
// HOW: Watch events trigger work queue items, which are processed
// by calling Reconcile() for each changed object.
//
// ...
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
```

### Testing
- Unit tests for all new functions
- Integration tests with envtest for controllers
- E2E tests for user-facing features

```bash
# Run unit tests
make test

# Run with coverage
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out
```

## Pull Request Guidelines

### PR Description
- Describe what changed and why
- Link to related issues
- Include testing steps

### Review Process
1. Automated checks must pass
2. Code review by maintainer
3. Address feedback
4. Merge when approved

## Questions?

Open an issue for any questions about contributing!
