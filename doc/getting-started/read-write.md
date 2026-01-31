# Read-Writing

We show here how you can open a package for both read/write operations.

We are going to use an instance `packaging` of `Packaging` to interact with the library.
Please see [entry-point.md](entry-point.md) for more details.

## Creating a New Package

Creating a new package is straightforward:

```go
pkg, err := packaging.Create("/path/to/some/file.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()
```

You can also create a package in a stream (for example, to write to a buffer or network connection):

```go
var buf bytes.Buffer
stream := ... // any io.ReadWriteSeeker

pkg, err := packaging.CreateInStream(stream)
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()
```

Please do not forget to close the package when done, or use `defer`.

## Opening a Package for Read/Writing

Opening a package for read/writing is similar to how we open a package for reading:

```go
pkg, err := packaging.OpenReadWrite("/path/to/some/file.aasx")
if err != nil {
    log.Fatal(err)
}
defer pkg.Close()
```

## Putting Parts Together

The [Open Packaging Conventions] format is based on parts and relationships.
The parts represent the pieces of data, while relationships model how these pieces relate to each other.

[Open Packaging Conventions]: https://en.wikipedia.org/wiki/Open_Packaging_Conventions

We wanted to encapsulate as much as possible the underlying format, but we decided to keep the structure of [Open Packaging Conventions].
This means that you need to first write a part, and then establish its relation to other parts (or package itself).

The parts are put to the package using `PutPart`:

```go
import "net/url"

uri, _ := url.Parse("/aasx/some-company/data.json")
content := []byte("{}")

part, err := pkg.PutPart(uri, "text/json", content)
if err != nil {
    log.Fatal(err)
}
```

You can also use streams:

```go
import "net/url"

uri, _ := url.Parse("/aasx/data.json")
stream := ... // any io.Reader

part, err := pkg.PutPartFromStream(uri, "text/json", stream)
if err != nil {
    log.Fatal(err)
}
```

The `PutPart` function returns a `*Part` so that you can easily chain it with other functions (see below for examples).

### Overwriting & Deleting

If a part already exists at the given URI, it is silently overwritten.
Therefore you need to be careful when you overwrite a part and make sure that the relationships are updated accordingly.

This also applies when deleting the parts.
Since it would be unfortunately inefficient to enforce consistency, the library indeed allows you to make your package inconsistent (where relationships point to dangling parts).

To delete a part:

```go
err := pkg.DeletePart(part)
if err != nil {
    log.Fatal(err)
}
```

### Specs

You establish a part as a spec by calling `MakeSpec`:

```go
uri, _ := url.Parse("/aasx/some-company/data.json")
content := []byte("{}")

part, err := pkg.PutPart(uri, "text/json", content)
if err != nil {
    log.Fatal(err)
}

err = pkg.MakeSpec(part)
if err != nil {
    log.Fatal(err)
}
```

To remove a spec relationship:

```go
err := pkg.UnmakeSpec(part)
if err != nil {
    log.Fatal(err)
}
```

## Supplementary Parts

Similar to how you make parts into specs, you relate supplementary parts to the spec parts with `RelateSupplementaryToSpec`:

```go
specURI, _ := url.Parse("/aasx/data.xml")
specContent := []byte("<aas>...</aas>")
spec, err := pkg.PutPart(specURI, "application/xml", specContent)
if err != nil {
    log.Fatal(err)
}
pkg.MakeSpec(spec)

supplURI, _ := url.Parse("/aasx-suppl/manual.pdf")
supplContent := ... // PDF bytes
supplementary, err := pkg.PutPart(supplURI, "application/pdf", supplContent)
if err != nil {
    log.Fatal(err)
}

err = pkg.RelateSupplementaryToSpec(spec, supplementary)
if err != nil {
    log.Fatal(err)
}
```

The relation can also be undone:

```go
err := pkg.UnrelateSupplementaryFromSpec(spec, supplementary)
if err != nil {
    log.Fatal(err)
}
```

## Thumbnail

There can be only one thumbnail per package.

Similar to specs, you set a thumbnail relation from a package to a part:

```go
thumbURI, _ := url.Parse("/thumbnail.png")
thumbContent := ... // PNG bytes

thumbnail, err := pkg.PutPart(thumbURI, "image/png", thumbContent)
if err != nil {
    log.Fatal(err)
}

err = pkg.SetThumbnail(thumbnail)
if err != nil {
    log.Fatal(err)
}
```

If you want to remove the thumbnail, call:

```go
err := pkg.UnsetThumbnail()
if err != nil {
    log.Fatal(err)
}
```

## Flushing

Flushing is necessary if you want to make sure that the changes you made to the package are persisted properly.

To flush:

```go
err := pkg.Flush()
if err != nil {
    log.Fatal(err)
}
```

This writes all pending changes to the file or stream.

## Concurrency

Please be careful if you read parts while you are writing.
The library uses internal locking for individual operations, but you need to handle higher-level synchronization yourself if performing complex operations across multiple goroutines.

Additionally, be careful to re-read the proper nuggets of information if you changed them.
For example, capturing the groups of specs by content type becomes invalid if you add a new spec:

```go
specsByContentType, _ := pkg.SpecsByContentType()

for contentType, specs := range specsByContentType {
    // If you call PutPart with a different content type here...
    pkg.PutPart(...)

    // specsByContentType is now stale and needs to be re-read.
}
```
