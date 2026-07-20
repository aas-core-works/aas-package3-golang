# Installation

The library is available as a Go module.

You can add it to your project using `go get`:

```bash
go get github.com/aas-core-works/aas-package3-golang/v2
```

Then import it in your code:

```go
import aasx "github.com/aas-core-works/aas-package3-golang/v2"
```

## Requirements

- Go 1.18 or later
- No external dependencies (standard library only)

## Migrating from v1

Version 2 uses Go semantic import versioning, so update imports to include `/v2`.
Read-only packages are now lazy: `OpenRead` keeps its file open until `Close`, and
payload CRC, decompression, or truncation errors can be returned when a part is read.
Consume and close all part streams before closing the package. Existing mutable
`PackageReadWrite` operations remain materialized and retain their v1 behavior.
