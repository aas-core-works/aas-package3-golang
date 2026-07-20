# API Documentation

This section provides an overview of the `aasx` package API.

For complete API documentation, run `go doc` on the package:

```bash
go doc github.com/aas-core-works/aas-package3-golang/v2
```

Or view it on [pkg.go.dev](https://pkg.go.dev/github.com/aas-core-works/aas-package3-golang/v2).

## Package Overview

```go
import aasx "github.com/aas-core-works/aas-package3-golang/v2"
```

## Constants

### Relation Types

The package defines the following OPC relationship types used in AASX packages:

| Constant | Value |
|----------|-------|
| `RelationTypeAasxOrigin` | `http://admin-shell.io/aasx/relationships/aasx-origin` |
| `RelationTypeAasxSpec` | `http://admin-shell.io/aasx/relationships/aas-spec` |
| `RelationTypeAasxSupplementary` | `http://admin-shell.io/aasx/relationships/aas-suppl` |
| `RelationTypeThumbnail` | `http://schemas.openxmlformats.org/package/2006/relationships/metadata/thumbnail` |

### Errors

| Error | Description |
|-------|-------------|
| `ErrNoOriginPart` | Returned when no AASX origin part is found |
| `ErrPartNotFound` | Returned when a requested part does not exist |
| `ErrInvalidFormat` | Returned when the package format is invalid |
| `ErrReaderLimitExceeded` | Returned when a configured reader limit is exceeded |
| `ErrPackageClosed` | Returned when reading a part after package closure |
| `ErrWriterClosed` | Returned when operating on a closed `PackageWriter` |
| `ErrWriteOnlyPart` | Returned when reading a `PackageWriter` part handle |

## Types

### Packaging

`Packaging` is the factory for opening and creating AASX packages.

```go
type Packaging struct{}

func NewPackaging() *Packaging
func (p *Packaging) Create(path string) (*PackageReadWrite, error)
func (p *Packaging) CreateInStream(stream io.ReadWriteSeeker) (*PackageReadWrite, error)
func (p *Packaging) CreateWriter(writer io.Writer) (*PackageWriter, error)
func (p *Packaging) OpenRead(path string, options ...ReaderOption) (*PackageRead, error)
func (p *Packaging) OpenReadFromStream(stream io.ReadSeeker, options ...ReaderOption) (*PackageRead, error)
func (p *Packaging) OpenReadFromReaderAt(reader io.ReaderAt, size int64, options ...ReaderOption) (*PackageRead, error)
func (p *Packaging) OpenReadWrite(path string) (*PackageReadWrite, error)
func (p *Packaging) OpenReadWriteFromStream(stream io.ReadWriteSeeker) (*PackageReadWrite, error)
```

### Reader Options

All reader limits use expanded byte sizes, and zero means unlimited.

```go
func WithMaxPartCount(max uint64) ReaderOption
func WithMaxOPCMetadataBytes(max uint64) ReaderOption
func WithMaxPartExpandedBytes(max uint64) ReaderOption
func WithMaxTotalExpandedBytes(max uint64) ReaderOption
```

### PackageRead

`PackageRead` provides read-only access to an AASX package.

```go
type PackageRead struct {
    Path string // File path (empty if opened from stream)
}

func (p *PackageRead) Close() error
func (p *PackageRead) Specs() ([]*Part, error)
func (p *PackageRead) SpecsByContentType() (map[string][]*Part, error)
func (p *PackageRead) IsSpec(part *Part) (bool, error)
func (p *PackageRead) SupplementariesFor(spec *Part) ([]*Part, error)
func (p *PackageRead) SupplementaryRelationships() ([]*SupplementaryRelationship, error)
func (p *PackageRead) FindPart(uri *url.URL) (*Part, error)
func (p *PackageRead) MustPart(uri *url.URL) (*Part, error)
func (p *PackageRead) Thumbnail() (*Part, error)
```

`PackageRead` lazily opens normal ZIP entries. `OpenRead` owns its file until
`Close`; externally supplied readers remain caller-owned. Consume part streams
before closing the package.

### PackageWriter

`PackageWriter` creates a new package incrementally on an `io.Writer`. It is
append-only and writes OPC metadata while closing.

```go
func (p *PackageWriter) PutPartFromStream(uri *url.URL, contentType string, stream io.Reader) (*Part, error)
func (p *PackageWriter) MakeSpec(part *Part) error
func (p *PackageWriter) RelateSupplementaryToSpec(supplementary *Part, spec *Part) error
func (p *PackageWriter) SetThumbnail(part *Part) error
func (p *PackageWriter) Close() error
```

The destination remains caller-owned. An I/O failure poisons the writer and can
leave partial ZIP output.

### PackageReadWrite

`PackageReadWrite` provides read and write access to an AASX package. It embeds `PackageRead` for read functionality.

```go
type PackageReadWrite struct {
    PackageRead
}

func (p *PackageReadWrite) PutPart(uri *url.URL, contentType string, content []byte) (*Part, error)
func (p *PackageReadWrite) PutPartFromStream(uri *url.URL, contentType string, stream io.Reader) (*Part, error)
func (p *PackageReadWrite) DeletePart(part *Part) error
func (p *PackageReadWrite) MakeSpec(part *Part) error
func (p *PackageReadWrite) UnmakeSpec(part *Part) error
func (p *PackageReadWrite) RelateSupplementaryToSpec(supplementary *Part, spec *Part) error
func (p *PackageReadWrite) UnrelateSupplementaryFromSpec(supplementary *Part, spec *Part) error
func (p *PackageReadWrite) SetThumbnail(part *Part) error
func (p *PackageReadWrite) UnsetThumbnail() error
func (p *PackageReadWrite) Flush() error
```

### Part

`Part` represents a part within an AASX package.

```go
type Part struct {
    URI         *url.URL // Location of the part within the package
    ContentType string   // MIME type of the part
}

func (p *Part) Stream() (io.ReadCloser, error)
func (p *Part) ReadAllBytes() ([]byte, error)
func (p *Part) ReadAllText() (string, error)
```

### SupplementaryRelationship

`SupplementaryRelationship` represents a relationship between a spec and its supplementary part.

```go
type SupplementaryRelationship struct {
    Spec          *Part
    Supplementary *Part
}
```

## Design-by-Contract

The package also includes design-by-contract utilities:

| Function | Description |
|----------|-------------|
| `Require(condition, message)` | Checks a precondition, panics if false |
| `Ensure(condition, message)` | Checks a postcondition, panics if false |

| Error | Description |
|-------|-------------|
| `ErrPreconditionViolation` | Returned/used when a precondition fails |
| `ErrPostconditionViolation` | Returned/used when a postcondition fails |
