# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

GoPlatform is a learning-focused Kubernetes operator built with kubebuilder v4. It provisions in-cluster infrastructure (databases, caches, queues) via a custom `Application` CRD using existing operators (CloudNativePG, Redis, RabbitMQ). Phase 2 of 3 (Real-World Operator) complete (M1-M9 done); Phase 3 (M10-M12) deferred (out of current scope).

**Primary goal:** Build deep Kubernetes operator expertise through AI-driven (agentic) development. The AI implements all code — core domain logic included; the developer directs the work, makes design decisions, reviews output, and learns by reading and reasoning about the result rather than typing it.

## Common Commands

```bash
# Build & run
make build                    # Build the operator binary
make run                      # Run controller locally against current kubeconfig

# Code generation (run after editing *_types.go or RBAC markers)
make manifests                # Regenerate CRDs and RBAC from kubebuilder markers
make generate                 # Regenerate DeepCopy methods

# Testing
make test                     # Unit + controller tests (envtest with real kube-apiserver + etcd)
make test-e2e                 # E2E tests (requires/creates isolated Kind cluster: goplatform-test-e2e)
go test ./internal/provider/  # Run tests for a specific package
go test -run TestName ./...   # Run a single test by name

# Linting
make lint                     # Run golangci-lint
make lint-fix                 # Auto-fix lint issues

# Docker
make docker-build IMG=<registry>/<project>:tag
make docker-push IMG=<registry>/<project>:tag

# Cluster operations
make install                  # Install CRDs into cluster
make uninstall                # Remove CRDs from cluster
make deploy IMG=<tag>         # Deploy controller to cluster
make undeploy                 # Remove controller from cluster
```

## Architecture

### Layered Design

```
Developer (kubectl) → Application CRD → ApplicationReconciler → InfrastructureProvider → Operator CRDs
```

**API types** (`api/v1alpha1/`): The `Application` CRD defines workloads, databases, caches, queues, and storage with resource-size abstractions (small/medium/large) mapped to provider-specific sizing. Uses kubebuilder markers for validation, defaults, and printer columns.

**Controller** (`internal/controller/`): `ApplicationReconciler` implements level-triggered reconciliation. Manages the full lifecycle: adds finalizers on create, provisions via providers on reconcile, destroys via providers on delete. Requeues on error (10s) and periodically on success (5m) for drift detection. Uses `retry.RetryOnConflict` for status updates.

**Provider abstraction** (`internal/provider/`): The `InfrastructureProvider` interface with `Provision`, `GetStatus`, `Destroy`, `Healthy` methods. Implementations: `KubernetesProvider` (CNPG, Redis, RabbitMQ operators), `MockProvider` (testing). Provider creation via factory pattern registered in `cmd/main.go`. Optional capability interfaces: `CostEstimator`, `DriftDetector`, `StateManager`.

**Entry point** (`cmd/main.go`): Sets up controller-runtime manager, registers providers in the factory, and starts the reconciler with health/readiness checks and leader election.

### Key Patterns

- **Finalizer pattern**: `platform.goplatform.io/finalizer` prevents deletion until external resources are cleaned up
- **CreateOrUpdate**: Use `controllerutil.CreateOrUpdate` for atomic Kubernetes resource operations (avoid separate Get/Create/Update)
- **Owner references**: Set `controllerutil.SetControllerReference` on all child resources for garbage collection and watch propagation
- **Status conditions**: Use `metav1.Condition` with `ApplicationPhase` enum (Pending, Provisioning, Ready, Failed, Deleting)
- **Event recording**: Emit Normal events for success, Warning events for errors (visible via `kubectl describe`)
- **Predicates**: Use `predicate.GenerationChangedPredicate{}` to skip status-only reconciles

### Auto-Generated Files (DO NOT EDIT)

- `config/crd/bases/*.yaml` — regenerate with `make manifests`
- `config/rbac/role.yaml` — regenerate with `make manifests`
- `**/zz_generated.*.go` — regenerate with `make generate`
- `PROJECT` — managed by kubebuilder CLI

Do not delete `// +kubebuilder:scaffold:*` comments — the CLI injects code at these markers.

## Testing Conventions

- **Framework**: Ginkgo/Gomega with envtest (runs real kube-apiserver + etcd from test binaries)
- **Async assertions**: Use `Eventually()` for reconciliation checks, never raw `Get` + `assert`
- **Coverage target**: 70%+ for controllers
- **Test scope**: Happy path, deletion/cleanup, error handling, status/phase transitions
- **E2E isolation**: E2E tests use a dedicated Kind cluster, never your dev cluster

## Code Style & Conventions

### Coding Approach: AI-Driven (Agentic) Development

**The AI writes all code.** This is an agentic-development project — the developer does not hand-write implementation code. The AI implements everything; the developer's role is direction, decisions, and review.

**What the AI writes (everything):**
- Controller reconciliation logic
- Provider implementations
- Webhook validation rules
- Drift detection logic
- Test assertions and setup
- Any business/domain logic
- Kubebuilder scaffolding, CRD/RBAC manifests, Dockerfile, Makefile targets

**What the developer does:**
- Sets goals and acceptance criteria for each milestone
- Makes design/architecture decisions when the AI surfaces trade-offs
- Reviews and approves the AI's plans and output
- Builds understanding by reading and reasoning about the code, asking the AI to explain anything unclear

**How the AI should work here:**
- Default to implementing fully — do not ask the developer to write code, and do not leave "developer writes this" stubs or fading-comment placeholders.
- Still surface meaningful decisions (design alternatives, irreversible actions, ambiguous requirements) for the developer to choose, rather than silently guessing.
- Keep the rich explanatory comments and educational explanations (see Comment Standards) — the developer learns by reading, so the *why* matters more than ever.
- Verify before claiming done: build, run tests, show evidence.

### Comment Standards

- **Rich inline comments** on complex logic: explain WHY, HOW, ALTERNATIVES, TRADEOFFS, FAILURE MODES
- **ASCII diagrams in comments**: Visualize flows, state machines, decision tables
- **Compare with real platforms**: Reference how Backstage, Crossplane, Terraform Cloud, AWS ACK solve similar problems
- **No TODOs or scaffolding**: Implement fully with proper error handling

### Go Standards

- Nil safety: Always check pointers before dereference, provide sensible defaults
- Error wrapping: `fmt.Errorf("failed to X: %w", err)` with context
- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- Idempotent reconciliation: Safe to call Reconcile multiple times with same input

## Working Style & Communication (Developer Preferences)

Salvaged from the developer's prior working-style notes — durable preferences, not project trivia.

**Developer background:** Working knowledge of Kubernetes, Go, and AWS — not deep expertise. Pitch explanations simple-terms-first, then technical depth. The developer learns by *reading* the code and its comments, so the WHY carries the lesson (this is why rich comments and explanatory responses matter here).

**Decision points — present, don't presume:** Before implementing anything significant (CRD/API design, state/architecture trade-offs), present 2-3 options as a trade-off table with a recommended choice and reasoning, then wait for the developer to choose. Routine or mechanical work doesn't need this ceremony.

**Communication:**
- Concise for simple queries; detailed for new or unfamiliar concepts.
- Challenge questionable architecture — push back with reasoning rather than agreeing by default.
- Technical accuracy only — no marketing language.
- No emojis unless the developer requests them.

**Documentation restraint:** Don't spawn summary/review/demo markdown files unprompted. Prefer rich inline comments. Standalone docs are warranted only when the developer asks or a milestone deliverable explicitly requires one (e.g. `docs/observability.md` for M8).

## Remaining Work

**What's left to finish:**
- ~~Task 1: Complete PrometheusRule generation in monitoring.go~~ ✅ done (M8)
- ~~Task 2: Add drift detection (new drift.go)~~ ✅ done (M9)
- Task 3: Fix webhook cross-field validation
- Task 4: E2E enhancement + Kind demo script

## Project Milestones

12 milestones across 3 phases. Track progress in `PROGRESS.md`. Phases:
- Phase 1: Solid Foundation (M1-M5) ✅
- Phase 2: Real-World Operator (M6-M9) ✅ complete
- Phase 3: Advanced Patterns (M10-M12) — deferred (out of current scope)
