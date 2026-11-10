# Contributing to TracePort

Thank you for contributing to TracePort. Bug reports, architecture proposals, benchmark improvements, and pull requests are welcome.

## Development Workflow

### Prerequisites
- Go 1.25+ (`CGO_ENABLED=0` compatible)
- Git

### Setup
1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR_USERNAME/traceport.git
   cd traceport
   ```
3. Create a feature or fix branch:
   ```bash
   git checkout -b feat/your-feature-name
   ```

### Running Verification & Benchmarks
All packages must build and pass tests with `CGO_ENABLED=0`:
```bash
CGO_ENABLED=0 go vet ./...
CGO_ENABLED=0 go test -v ./...
CGO_ENABLED=0 go test -bench=. -benchmem ./pkg/checkpoint/...
gofmt -s -w .
```

### Pull Request Guidelines
- Keep pull requests focused on a well-defined change.
- Use Conventional Commits (`feat:`, `fix:`, `perf:`, `docs:`, `test:`).
- Ensure `go vet ./...` and `go test ./...` pass across Linux, macOS, and Windows.
