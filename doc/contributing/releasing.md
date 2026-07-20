# Releasing

## Versioning

We follow [Semantic Versioning].
The version X.Y.Z indicates:

* X is the major version (backward-incompatible),
* Y is the minor version (backward-compatible), and
* Z is the patch version (backward-compatible bug fix).

[Semantic Versioning]: http://semver.org/spec/v1.0.0.html

## Creating a Release

Releases are created using Git tags:

1. **Update version** (if needed in any documentation)

2. **Create a tag**:
   ```bash
   git tag v1.2.3
   ```

3. **Push the tag**:
   ```bash
   git push origin v1.2.3
   ```

4. **Create a GitHub Release**:
   - Go to the repository's Releases page
   - Click "Draft a new release"
   - Select the tag you just created
   - Add release notes describing the changes
   - Publish the release

## Go Module Versioning

Go modules use Git tags for versioning. Users can then install specific versions:

```bash
go get github.com/aas-core-works/aas-package3-golang/v2@v2.0.0
```

For major versions 2 and above, the import path must include the version:

```go
import aasx "github.com/aas-core-works/aas-package3-golang/v2"
```

## Release Checklist

Before creating a release:

1. ✅ All tests pass
2. ✅ Documentation is up to date
3. ✅ CHANGELOG (if maintained) is updated
4. ✅ Version number follows semantic versioning
5. ✅ No breaking changes in minor/patch versions
