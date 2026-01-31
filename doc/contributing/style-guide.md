# Style Guide

We follow the standard Go coding conventions and use `gofmt` for formatting.

## Formatting

All code must be formatted with `gofmt`:

```bash
go fmt ./...
```

## Linting

We recommend using [golangci-lint] for comprehensive linting:

[golangci-lint]: https://golangci-lint.run/

```bash
golangci-lint run
```

## Line Width

While Go doesn't enforce a strict line width, we recommend keeping lines under **100 characters** where practical.

This makes it easier to read code and allows for side-by-side diffs.

## Error Handling

* Always check and handle errors
* Return errors with context using `fmt.Errorf("context: %w", err)`
* Use the predefined error variables (`ErrNoOriginPart`, `ErrPartNotFound`, etc.) where appropriate

Example:

```go
pkg, err := packaging.OpenRead(path)
if err != nil {
    return nil, fmt.Errorf("failed to open package %s: %w", path, err)
}
```

## Naming Conventions

* Use `MixedCaps` or `mixedCaps` (camelCase) for names
* Exported names start with a capital letter
* Acronyms should be all caps (`URI`, `URL`, `XML`)
* Prefer short, descriptive names

## Comments

* Write doc comments for all exported types, functions, and methods
* Start comments with the name of the thing being documented
* Use complete sentences

Example:

```go
// Part represents a part of an AAS package.
// It provides methods to read the content as bytes, text, or a stream.
type Part struct {
    // URI is the location of the part within the package.
    URI *url.URL
    
    // ContentType is the MIME type of the part.
    ContentType string
}
```

## Testing

* Put tests in the same package with `_test.go` suffix
* Use table-driven tests where appropriate
* Test both success and error cases
* Use meaningful test names that describe what is being tested

Example:

```go
func TestOpenReadReturnsErrorForInvalidPackage(t *testing.T) {
    // ...
}
```

## Pre-conditions and Post-conditions

Use the `Require` and `Ensure` functions for design-by-contract checks:

```go
func doSomething(value int) {
    Require(value > 0, "value must be positive")
    // ...
    Ensure(result != nil, "result should not be nil")
}
```

## TODOs

Please do not leave any TODOs in the code.
Write a proper note with your username:

```go
// NOTE(your-username): Explanation of why this is done this way...
```
