# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o cat2k .

# Runtime stage
FROM alpine:3.19

WORKDIR /app

# Install ca-certificates for HTTPS API calls
RUN apk --no-cache add ca-certificates tzdata

# Copy binary from builder
COPY --from=builder /build/cat2k .

# Copy web assets
COPY web/ ./web/

# Create directory for database
RUN mkdir -p /data

# Default environment
ENV WEENECT_LOG_LEVEL=info

EXPOSE 8080

# Run the daemon
CMD ["./cat2k", "run"]
