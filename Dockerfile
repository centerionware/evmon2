# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app

# Copy everything first
COPY . .

# Generate go.sum and download all dependencies
RUN go mod tidy

# Build the binary
RUN go build -o evmon ./cmd/main.go

# Final stage
FROM alpine:3.18
WORKDIR /app
COPY --from=builder /app/evmon .
EXPOSE 8080
CMD ["./evmon"]