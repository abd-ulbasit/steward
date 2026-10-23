# Base image for the devcontainer, kept in a Dockerfile (rather than the
# "image" field of devcontainer.json) so Dependabot's docker ecosystem can see
# and update the digest pin. Keep the Go version in step with go.mod.
#
# Development only, never shipped: it runs as root because post-install.sh
# installs tools into /usr/local/bin (Trivy DS-0002 and DS-0026 are expected).
FROM golang:1.26.10@sha256:672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82
