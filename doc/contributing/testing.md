# Testing

## Running Tests

To run all tests:

```bash
go test ./...
```

To run tests with verbose output:

```bash
go test -v ./...
```

To run tests with coverage:

```bash
go test -cover ./...
```

To generate a coverage report:

```bash
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

## Test Resources

Test resources are stored in the `TestResources/` directory at the root of the repository.

The test resources include sample AASX packages from various vendors for testing read functionality:

```
TestResources/
└── TestPackageRead/
    ├── 01_Festo/
    ├── 02_Bosch/
    ├── 03_Bosch/
    └── ...
```

Each test resource directory contains:
- The AASX file (as XML)
- `specsTable.txt` - Expected specs information
- `supplementariesTable.txt` - Expected supplementary relationships
- `thumbnail.txt` - Expected thumbnail information

## Writing Tests

### Test File Organization

Test files are named `*_test.go` and placed in the same package:

```
package.go
package_test.go
dbc.go
dbc_test.go
```

### Test Helpers

Common test helpers are defined in the test files:

```go
// temporaryDirectory creates a temporary directory and returns its path
// along with a cleanup function.
func temporaryDirectory(t *testing.T) (string, func()) {
    t.Helper()
    dir, err := os.MkdirTemp("", "aasx-test-*")
    if err != nil {
        t.Fatalf("Failed to create temporary directory: %v", err)
    }
    return dir, func() {
        os.RemoveAll(dir)
    }
}

// Usage:
func TestSomething(t *testing.T) {
    tmpdir, cleanup := temporaryDirectory(t)
    defer cleanup()
    
    // Use tmpdir...
}
```

### Table-Driven Tests

For testing multiple similar cases, use table-driven tests:

```go
func TestPartReadAllText(t *testing.T) {
    tests := []struct {
        name     string
        content  []byte
        expected string
    }{
        {"empty", []byte{}, ""},
        {"ascii", []byte("hello"), "hello"},
        {"unicode", []byte("héllo"), "héllo"},
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            part := &Part{content: tt.content}
            got, err := part.ReadAllText()
            if err != nil {
                t.Fatalf("unexpected error: %v", err)
            }
            if got != tt.expected {
                t.Errorf("got %q, want %q", got, tt.expected)
            }
        })
    }
}
```

### Testing Error Cases

Always test error cases:

```go
func TestOpenReadReturnsErrorForNonExistentFile(t *testing.T) {
    packaging := NewPackaging()

    pkg, err := packaging.OpenRead("/this/path/does/not/exist.aasx")
    if err == nil {
        defer pkg.Close()
        t.Error("Expected error when opening non-existent file, got nil")
    }
}
```
