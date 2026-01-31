# Continuous Integration

To establish confidence in the software as well as to continuously maintain the code quality, we provide scripts to run checks locally and GitHub Actions workflows to run remotely.

## Running Checks Locally

### Building

To build the library:

```bash
go build -v ./...
```

### Testing

To run all tests:

```bash
go test ./...
```

To run tests with race detection (recommended for CI):

```bash
go test -race ./...
```

To run tests with coverage:

```bash
go test -covermode atomic -coverprofile=covprofile ./...
```

### Formatting

To check if code is properly formatted:

```bash
gofmt -l .
```

This lists files that need formatting. To format in place:

```bash
go fmt ./...
```

### Vetting

To run the Go vet tool:

```bash
go vet ./...
```

### Static Analysis

We use [staticcheck] for additional static analysis:

[staticcheck]: https://staticcheck.io/

```bash
# Install staticcheck
go install honnef.co/go/tools/cmd/staticcheck@latest

# Run staticcheck
staticcheck ./...
```

## GitHub Actions

GitHub Actions runs continuous integration automatically on pull requests and pushes to `main`.

The workflows are defined in [`.github/workflows/`]:

[`.github/workflows/`]: https://github.com/aas-core-works/aas-package3-golang/tree/main/.github/workflows

### test.yml

Runs on every push and pull request to `main`:

1. **Build** - Compiles the code
2. **Test** - Runs all unit tests with race detection
3. **Coverage** - Generates coverage report and sends to Coveralls

Tests are run against multiple Go versions (1.18, 1.19, 1.20, 1.21, 1.22, 1.23) to ensure compatibility.

### check-style.yml

Runs on every push and pull request to `main`:

1. **Format Check** - Verifies code is formatted with `gofmt`
2. **Vet** - Runs `go vet` for static analysis
3. **Staticcheck** - Runs additional static analysis

### check-doc.yml

Runs on every push and pull request to `main`:

1. **Go Doc** - Verifies that `go doc` can parse the package
2. **Markdown Links** - Checks for broken links in documentation

## Pre-Commit Checklist

Before pushing your changes, ensure:

1. ✅ Code compiles: `go build -v ./...`
2. ✅ Tests pass: `go test -race ./...`
3. ✅ Code is formatted: `go fmt ./...`
4. ✅ No vet warnings: `go vet ./...`
5. ✅ No staticcheck warnings: `staticcheck ./...`
6. ✅ Commit message follows guidelines
