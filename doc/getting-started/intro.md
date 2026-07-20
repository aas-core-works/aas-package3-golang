# Introduction

The following articles describe how you can install and use the library.

The library is written in pure Go and requires Go 1.18 or later.

Unless a version is indicated as a pre-release, the library has been thoroughly tested and is thus ready for use in production.

For details of the specification, please consult the [Details of the Asset Administration Shell v3].

[Details of the Asset Administration Shell v3]: https://www.plattform-i40.de/PI40/Redaktion/DE/Downloads/Publikation/Details_of_the_Asset_Administration_Shell_Part1_V3.pdf?__blob=publicationFile&v=5

## Package Overview

The `aasx` package provides the following key types:

| Type | Description |
|------|-------------|
| `Packaging` | Factory for opening and creating AASX packages |
| `PackageRead` | Read-only access to an AASX package |
| `PackageReadWrite` | Read and write access to an AASX package |
| `PackageWriter` | Append-only, bounded-memory package creation |
| `Part` | Represents a part within an AASX package |

## Error Handling

The library follows Go's idiomatic error handling pattern. Functions return `error` as the last return value, which should be checked by the caller:

```go
pkg, err := packaging.OpenRead("example.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()
```
