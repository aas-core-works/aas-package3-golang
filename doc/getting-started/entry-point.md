# Entry Point

The `Packaging` struct provides the main entry to the library:

```go
packaging := aasx.NewPackaging()
```

We decided not to use package-level functions to allow for mocking (in client tests) as well as to avoid problems in the future if we are to add configuration options.

All operations on packages are performed using the resulting `packaging` instance.

## Opening and Creating Packages

The `Packaging` instance provides the following methods:

| Method | Description |
|--------|-------------|
| `Create(path)` | Create a new AASX package at the given file path |
| `CreateInStream(stream)` | Create a new AASX package in a stream |
| `OpenRead(path)` | Open an existing AASX package for reading |
| `OpenReadFromStream(stream)` | Open an AASX package from a stream for reading |
| `OpenReadWrite(path)` | Open an existing AASX package for read/write |
| `OpenReadWriteFromStream(stream)` | Open an AASX package from a stream for read/write |

### Example

```go
packaging := aasx.NewPackaging()

// Open for reading
pkg, err := packaging.OpenRead("example.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()

// ... work with the package
```
