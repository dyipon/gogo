FROM golang:1.21-alpine

WORKDIR /app

# Install required system packages
RUN apk add --no-cache gcc musl-dev

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN go build -o pod-metrics-collector

# Run the application
CMD ["./pod-metrics-collector"] 