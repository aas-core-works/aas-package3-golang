# Reading

Using the `packaging` instance of `Packaging` (see [entry-point.md](entry-point.md)), we open a package for reading:

```go
packaging := aasx.NewPackaging()

pkg, err := packaging.OpenRead("/path/to/some/file.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()
```

The function parses the ZIP directory and OPC metadata, but leaves normal parts compressed
until they are read. It returns errors for the file, ZIP directory, and OPC metadata while
opening. Payload CRC, decompression, and truncation errors are returned later by the part
stream or a `ReadAll` convenience method.

`OpenRead` owns an open file until `Close`. Readers passed to `OpenReadFromReaderAt` or
`OpenReadFromStream` remain caller-owned, but they must stay usable until the package is
closed. Consume and close all part streams before closing the package.

For untrusted packages, configure limits while opening:

```go
pkg, err := packaging.OpenRead(
    "/path/to/some/file.aasx",
    aasx.WithMaxPartCount(10_000),
    aasx.WithMaxOPCMetadataBytes(16<<20),
    aasx.WithMaxPartExpandedBytes(1<<30),
    aasx.WithMaxTotalExpandedBytes(4<<30),
)
```

A zero limit means unlimited, which is the default.

## Parts

Parts are the cornerstone of a package indicating a unit, analogous to files in a file system.
We distinguish between three categories of parts in the context of an AAS package:

* Specs,
* Supplementaries, and
* Thumbnail.

Each part has a content type (given as a [MIME type]) and gives you access to its content.

[MIME type]: https://developer.mozilla.org/en-US/docs/Web/HTTP/Basics_of_HTTP/MIME_types

A part is modeled as an instance of the `Part` struct.
You can read all bytes or all text from it, or open a read stream:

```go
part := ... // obtained from package

// Read all bytes
content, err := part.ReadAllBytes()
if err != nil {
    log.Fatal(err)
}

// Read as text
text, err := part.ReadAllText()
if err != nil {
    log.Fatal(err)
}

// Open a stream
stream, err := part.Stream()
if err != nil {
    log.Fatal(err)
}
defer stream.Close()
// Read from stream...
```

If you open a stream, close it before closing the package.

### Specs

Specs define the data of your administration shell.

You can list all the specs available in the package with:

```go
specs, err := pkg.Specs()
if err != nil {
    log.Fatal(err)
}

for _, spec := range specs {
    fmt.Printf("Spec: %s (%s)\n", spec.URI, spec.ContentType)
    // do something with spec
}
```

You can also group the specs by their content type:

```go
specsByContentType, err := pkg.SpecsByContentType()
if err != nil {
    log.Fatal(err)
}

if jsonSpecs, ok := specsByContentType["text/json"]; ok {
    spec := jsonSpecs[0]
    // Do something with JSON spec
} else if xmlSpecs, ok := specsByContentType["application/xml"]; ok {
    spec := xmlSpecs[0]
    // Do something with XML spec
} else {
    // Report that we could not find neither JSON nor XML
}
```

According to [Details of the Asset Administration Shell v3], specs should all represent the same data model albeit in a different format.
Multiple equivalent models per content type are also possible.

## Supplementary Materials

If you know the URI of a supplementary part within the package, you can directly access it:

```go
import "net/url"

uri, _ := url.Parse("/aasx/suppl/something.pdf")
suppl, err := pkg.FindPart(uri)
if err != nil {
    log.Fatal(err)
}

if suppl != nil {
    // Do something with the supplementary part.
    // For example, read all the bytes.
    content, err := suppl.ReadAllBytes()
    if err != nil {
        log.Fatal(err)
    }
    // ...
}
```

If the part must exist, use `MustPart` instead which returns an error if not found:

```go
uri, _ := url.Parse("/aasx/suppl/something.pdf")
suppl, err := pkg.MustPart(uri)
if err != nil {
    log.Fatal(err) // Part not found
}
// suppl is guaranteed to be non-nil
```

Otherwise, if you want to inspect all the supplementary parts for a given spec, you can list them:

```go
specs, err := pkg.Specs()
if err != nil {
    log.Fatal(err)
}

for _, spec := range specs {
    suppls, err := pkg.SupplementariesFor(spec)
    if err != nil {
        log.Fatal(err)
    }
    
    for _, suppl := range suppls {
        fmt.Printf("Supplementary: %s\n", suppl.URI)
        // Do something with suppl
    }
}
```

You can also iterate over all supplementary relationships:

```go
relationships, err := pkg.SupplementaryRelationships()
if err != nil {
    log.Fatal(err)
}

for _, rel := range relationships {
    fmt.Printf("Spec: %s -> Supplementary: %s\n", 
        rel.Spec.URI, rel.Supplementary.URI)
}
```

## Thumbnail

Here is how to query for a thumbnail of the package:

```go
thumb, err := pkg.Thumbnail()
if err != nil {
    log.Fatal(err)
}

if thumb != nil {
    // Do something with the thumbnail.
    // For example, read all the bytes.
    content, err := thumb.ReadAllBytes()
    if err != nil {
        log.Fatal(err)
    }
    // ...
}
```
