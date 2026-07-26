# Build stage
FROM golang:1.25-alpine AS builder

# Install dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /app

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the operator binary
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags='-w -s -extldflags "-static"' \
    -o /manager \
    ./cmd/

# Runtime stage
FROM alpine:3.19

# Install ca-certificates and terraform
RUN apk add --no-cache ca-certificates tzdata

# Copy Terraform binary (will be installed in CI/build)
# For production, install terraform here or mount it
# RUN wget -O /tmp/terraform.zip https://releases.hashicorp.com/terraform/1.6.6/terraform_1.6.6_linux_amd64.zip && \
#     unzip /tmp/terraform.zip -d /usr/local/bin/ && \
#     rm /tmp/terraform.zip

# Create non-root user with explicit numeric UID/GID.
# Kubernetes runAsNonRoot requires a numeric user to verify non-root status.
RUN adduser -D -u 65532 -g '' steward

# Copy binary from builder
COPY --from=builder /manager /manager

# Use numeric UID so Kubernetes can verify runAsNonRoot
USER 65532

# The deployment manifest sets the command to /manager
ENTRYPOINT ["/manager"]
