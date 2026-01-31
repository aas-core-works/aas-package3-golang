# aas-package3-golang

[![Test](https://github.com/aas-core-works/aas-package3-golang/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/aas-core-works/aas-package3-golang/actions/workflows/test.yml)
[![Check style](https://github.com/aas-core-works/aas-package3-golang/actions/workflows/check-style.yml/badge.svg)](https://github.com/aas-core-works/aas-package3-golang/actions/workflows/check-style.yml)
[![Coverage Status](https://coveralls.io/repos/github/aas-core-works/aas-package3-golang/badge.svg?branch=main)](https://coveralls.io/github/aas-core-works/aas-package3-golang?branch=main)
[![Go Reference](https://pkg.go.dev/badge/github.com/aas-core-works/aas-package3-golang.svg)](https://pkg.go.dev/github.com/aas-core-works/aas-package3-golang)

Aas-package3-golang is a library for reading and writing packaged file format of an [Asset Administration Shell (AAS)] in Go.

[Asset Administration Shell (AAS)]: https://www.plattform-i40.de/PI40/Redaktion/DE/Downloads/Publikation/Details_of_the_Asset_Administration_Shell_Part1_V3.html

## Status

The library is thoroughly tested and meant to be used in production.

The library is written in pure Go with no external dependencies beyond the standard library. Go 1.18 or later is required.

## Documentation

The full documentation is available at [doc/index.md](doc/index.md).

### Teaser

Here are short snippets to demonstrate how you can use the library.

To create and write to a package:

```go
import (
    "net/url"
    aasx "github.com/aas-core-works/aas-package3-golang"
)

// General packaging handler to be shared across the program
packaging := aasx.NewPackaging()

// Create a package
specContent := []byte(`{"aas": "..."}`)
thumbnailContent := []byte{...} // PNG bytes
supplementaryContent := []byte{...} // PDF bytes

pkg, err := packaging.Create("/path/to/some/file.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()

specURI, _ := url.Parse("/aasx/some-company/data.json")
spec, _ := pkg.PutPart(specURI, "text/json", specContent)
pkg.MakeSpec(spec)

thumbURI, _ := url.Parse("/some-thumbnail.png")
thumb, _ := pkg.PutPart(thumbURI, "image/png", thumbnailContent)
pkg.SetThumbnail(thumb)

supplURI, _ := url.Parse("/aasx-suppl/some-company/some-manual.pdf")
suppl, _ := pkg.PutPart(supplURI, "application/pdf", supplementaryContent)
pkg.RelateSupplementaryToSpec(spec, suppl)

pkg.Flush()
```

To read from the package:

```go
import (
    "net/url"
    aasx "github.com/aas-core-works/aas-package3-golang"
)

// General packaging handler to be shared across the program
packaging := aasx.NewPackaging()

// Read from the package
pkg, err := packaging.OpenRead("/path/to/some/file.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()

// Read the specs
specsByContentType, _ := pkg.SpecsByContentType()
if jsonSpecs, ok := specsByContentType["text/json"]; ok {
    spec := jsonSpecs[0]
    specContent, _ := spec.ReadAllBytes()
    // Do something with the spec content
}

// Read the thumbnail
thumbnail, _ := pkg.Thumbnail()
if thumbnail != nil {
    thumbnailContent, _ := thumbnail.ReadAllBytes()
    // Do something with the thumbnail content
}

// Read a supplementary file
supplURI, _ := url.Parse("/aasx-suppl/some-company/some-manual.pdf")
suppl, err := pkg.MustPart(supplURI)
if err != nil {
    log.Fatal(err)
}
supplementaryContent, _ := suppl.ReadAllBytes()
```

Please see the full documentation at [doc/index.md](doc/index.md) for more details.

## Installation

```bash
go get github.com/aas-core-works/aas-package3-golang
```

## API Overview

### Types

| Type | Description |
|------|-------------|
| `Packaging` | Factory for opening and creating AASX packages |
| `PackageRead` | Read-only access to an AASX package |
| `PackageReadWrite` | Read and write access to an AASX package |
| `Part` | Represents a part within an AASX package |

### Packaging Methods

| Method | Description |
|--------|-------------|
| `Create(path)` | Create a new AASX package at the given path |
| `CreateInStream(stream)` | Create a new AASX package in a stream |
| `OpenRead(path)` | Open an AASX package for reading |
| `OpenReadFromStream(stream)` | Open an AASX package from a stream for reading |
| `OpenReadWrite(path)` | Open an AASX package for read/write |
| `OpenReadWriteFromStream(stream)` | Open an AASX package from a stream for read/write |

### PackageRead Methods

| Method | Description |
|--------|-------------|
| `Specs()` | List all AAS spec parts |
| `SpecsByContentType()` | List specs grouped by MIME type |
| `IsSpec(part)` | Check if a part is a spec |
| `SupplementariesFor(spec)` | List supplementary parts for a spec |
| `SupplementaryRelationships()` | List all supplementary relationships |
| `FindPart(uri)` | Find a part by URI (returns nil if not found) |
| `MustPart(uri)` | Get a part by URI (returns error if not found) |
| `Thumbnail()` | Get the package thumbnail |
| `Close()` | Close the package |

### PackageReadWrite Methods

Inherits all `PackageRead` methods, plus:

| Method | Description |
|--------|-------------|
| `PutPart(uri, contentType, content)` | Write a part to the package |
| `PutPartFromStream(uri, contentType, stream)` | Write a part from a stream |
| `DeletePart(part)` | Remove a part from the package |
| `MakeSpec(part)` | Mark a part as a spec |
| `UnmakeSpec(part)` | Remove spec relationship |
| `RelateSupplementaryToSpec(spec, supplementary)` | Create supplementary relationship |
| `UnrelateSupplementaryFromSpec(spec, supplementary)` | Remove supplementary relationship |
| `SetThumbnail(part)` | Set the package thumbnail |
| `UnsetThumbnail()` | Remove the package thumbnail |
| `Flush()` | Write pending changes |

## Versioning

The name of the library indicates the supported version of the [Asset Administration Shell (AAS)].

In case of `aas-package3-golang`, this means that Version 3 of the [Asset Administration Shell (AAS)] is supported.

We follow [Semantic Versioning] to version the library.
The version X.Y.Z indicates:

[Semantic Versioning]: http://semver.org/spec/v1.0.0.html

* X is the major version (backward-incompatible),
* Y is the minor version (backward-compatible), and
* Z is the patch version (backward-compatible bug fix).
