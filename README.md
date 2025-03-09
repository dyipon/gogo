# Kubernetes Pod Metrics Collector

A lightweight monitoring application that collects and visualizes CPU and memory metrics from Kubernetes pods. The application provides a dark-themed web interface for real-time monitoring and historical data analysis.

This application was developed with the assistance of Claude (Anthropic), an AI coding assistant. The entire codebase was generated through natural language interactions, demonstrating the capabilities of AI-assisted development while maintaining high code quality and following best practices.

## Features
- Real-time monitoring of pod CPU and memory usage
- Historical metrics with 24-hour graphs
- Interactive charts with zoom and pan capabilities
- Pod filtering and metric sorting
- Maximum resource usage tracking
- Dark mode interface

## Usage

### Running with Docker
```bash
docker build -t pod-metrics-collector .
docker run -d \
  -p 8080:8080 \
  -v ~/.kube/config:/root/.kube/config \
  -v $(pwd)/pod_metrics.db:/app/pod_metrics.db \
  pod-metrics-collector
```

### Running directly
```bash
go build -o pod-metrics-collector
./pod-metrics-collector [-d] # -d for daemon mode
```

The web interface will be available at http://192.168.13.1:8080/metrics 