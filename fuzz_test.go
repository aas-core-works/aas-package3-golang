package aasx

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/url"
	pathpkg "path"
	"strings"
	"testing"
)

func limitBytes(b []byte, max int) []byte {
	if len(b) > max {
		return b[:max]
	}
	return b
}

func fuzzPath(prefix, name string) *url.URL {
	if name == "" {
		name = "empty"
	}
	hexName := hex.EncodeToString([]byte(name))
	uri, _ := url.Parse("/aasx/" + prefix + "-" + hexName)
	return uri
}

func FuzzPathHelpers(f *testing.F) {
	f.Add("_rels/.rels", "target")
	f.Add("aasx/_rels/aasx-origin.rels", "../suppl/data.bin")
	f.Add("/aasx/data.txt", "/absolute/target.txt")

	f.Fuzz(func(t *testing.T, relsPath string, target string) {
		sourcePath := getSourcePathFromRelsPath(relsPath)
		resolved := resolveRelativeURI(sourcePath, target)
		if target != "" && strings.HasPrefix(target, "/") {
			expected := strings.ReplaceAll(target, "\\", "/")
			expected = pathpkg.Clean(expected)
			if !strings.HasPrefix(expected, "/") {
				expected = "/" + expected
			}
			if resolved != expected {
				t.Fatalf("expected canonical absolute target %q, got %q", expected, resolved)
			}
		} else {
			if resolved != "" && !strings.HasPrefix(resolved, "/") {
				t.Fatalf("expected resolved path to start with '/', got %q", resolved)
			}
		}
		if strings.Contains(resolved, "\\") {
			t.Fatalf("expected resolved path to use forward slashes, got %q", resolved)
		}

		normalized := normalizePathForMap(sourcePath)
		if sourcePath != "" && !strings.HasPrefix(normalized, "/") {
			t.Fatalf("expected normalized path to start with '/', got %q", normalized)
		}
		if normalized != strings.ToLower(normalized) {
			t.Fatalf("expected normalized path to be lowercase, got %q", normalized)
		}

		if strings.HasSuffix(relsPath, ".rels") && strings.Contains(relsPath, "_rels") {
			relsRoundTrip := getRelsPath(sourcePath)
			if !strings.HasSuffix(relsRoundTrip, ".rels") {
				t.Fatalf("expected rels path to end with .rels, got %q", relsRoundTrip)
			}
			if !strings.Contains(relsRoundTrip, "_rels") {
				t.Fatalf("expected rels path to include _rels, got %q", relsRoundTrip)
			}
		}
	})
}

func FuzzOpenReadFromBytes(f *testing.F) {
	f.Add([]byte("not a zip"))
	f.Add([]byte("PK\x03\x04"))

	f.Fuzz(func(t *testing.T, data []byte) {
		packaging := NewPackaging()

		reader := bytes.NewReader(data)
		pkg, err := packaging.OpenReadFromStream(reader)
		if err == nil {
			_ = pkg.Close()
		}

		buf := bytes.NewBuffer(append([]byte{}, data...))
		stream := &readWriteSeeker{buf: buf}
		pkgRW, err := packaging.OpenReadWriteFromStream(stream)
		if err == nil {
			_ = pkgRW.Close()
		}
	})
}

func FuzzRoundTripPackage(f *testing.F) {
	f.Add("spec", "suppl", "thumb", []byte("spec"), []byte("suppl"), []byte("thumb"))

	f.Fuzz(func(t *testing.T, specName string, supplName string, thumbName string, specContent []byte, supplContent []byte, thumbContent []byte) {
		packaging := NewPackaging()
		stream := &readWriteSeeker{buf: &bytes.Buffer{}}

		pkg, err := packaging.CreateInStream(stream)
		if err != nil {
			t.Fatalf("failed to create package: %v", err)
		}

		specContent = limitBytes(specContent, 1024)
		supplContent = limitBytes(supplContent, 1024)
		thumbContent = limitBytes(thumbContent, 1024)

		specURI := fuzzPath("spec", specName)
		supplURI := fuzzPath("suppl", supplName)
		thumbURI := fuzzPath("thumb", thumbName)

		specPart, err := pkg.PutPart(specURI, "application/octet-stream", specContent)
		if err != nil {
			t.Fatalf("failed to put spec: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(supplURI, "application/octet-stream", supplContent)
		if err != nil {
			t.Fatalf("failed to put supplementary: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("failed to relate supplementary: %v", err)
		}

		thumbPart, err := pkg.PutPart(thumbURI, "application/octet-stream", thumbContent)
		if err != nil {
			t.Fatalf("failed to put thumbnail: %v", err)
		}
		if err := pkg.SetThumbnail(thumbPart); err != nil {
			t.Fatalf("failed to set thumbnail: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("failed to close: %v", err)
		}

		if _, err := stream.Seek(0, io.SeekStart); err != nil {
			t.Fatalf("failed to seek: %v", err)
		}

		readPkg, err := packaging.OpenReadFromStream(stream)
		if err != nil {
			t.Fatalf("failed to open package: %v", err)
		}
		defer readPkg.Close()

		specs, err := readPkg.Specs()
		if err != nil {
			t.Fatalf("failed to read specs: %v", err)
		}
		if len(specs) != 1 {
			t.Fatalf("expected 1 spec, got %d", len(specs))
		}

		storedSpec, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("failed to read spec: %v", err)
		}
		if !bytes.Equal(storedSpec, specContent) {
			t.Fatalf("spec content mismatch")
		}

		rels, err := readPkg.SupplementaryRelationships()
		if err != nil {
			t.Fatalf("failed to read supplementaries: %v", err)
		}
		if len(rels) != 1 {
			t.Fatalf("expected 1 supplementary, got %d", len(rels))
		}

		storedSuppl, err := rels[0].Supplementary.ReadAllBytes()
		if err != nil {
			t.Fatalf("failed to read supplementary: %v", err)
		}
		if !bytes.Equal(storedSuppl, supplContent) {
			t.Fatalf("supplementary content mismatch")
		}

		thumb, err := readPkg.Thumbnail()
		if err != nil {
			t.Fatalf("failed to read thumbnail: %v", err)
		}
		if thumb == nil {
			t.Fatalf("expected thumbnail")
		}

		storedThumb, err := thumb.ReadAllBytes()
		if err != nil {
			t.Fatalf("failed to read thumbnail: %v", err)
		}
		if !bytes.Equal(storedThumb, thumbContent) {
			t.Fatalf("thumbnail content mismatch")
		}
	})
}
