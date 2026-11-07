# Contributing to TracePort

Thank you for your interest in contributing to **TracePort**! We welcome bug reports, feature suggestions, documentation updates, and pull requests.

## Development Workflow

### Prerequisites
- **Go 1.22+**
- Git

### Setup
1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR_USERNAME/traceport.git
   cd traceport
   ```
3. Create a descriptive feature or bugfix branch:
   ```bash
   git checkout -b feat/your-feature-name
   ```

### Running Tests & Linting
Ensure all tests pass and formatting is clean:
```bash
go test -v ./...
go vet ./...
gofmt -s -w .
```

### Pull Request Guidelines
- Keep pull requests focused on a single change or feature.
- Ensure your branch is rebased on the latest `main`.
- Fill out the Pull Request template completely.
- Verify all CI checks pass.
