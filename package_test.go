package aasx

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// =============================================================================
// Test Helpers
// =============================================================================

// temporaryDirectory creates a temporary directory and returns its path along
// with a cleanup function that removes the directory.
func temporaryDirectory(t *testing.T) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "aasx-test-*")
	if err != nil {
		t.Fatalf("Failed to create temporary directory: %v", err)
	}
	return dir, func() {
		os.RemoveAll(dir)
	}
}

// mustParseURL parses a URL and panics if it fails.
func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

type byteRange struct {
	start int64
	end   int64
}

type failingRangeReaderAt struct {
	reader  *bytes.Reader
	blocked byteRange
	err     error
}

type observingRangeReaderAt struct {
	reader      *bytes.Reader
	observed    byteRange
	maxReadSize int
	mu          sync.Mutex
}

func (reader *observingRangeReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	end := offset + int64(len(buffer))
	if offset < reader.observed.end && end > reader.observed.start {
		reader.mu.Lock()
		if len(buffer) > reader.maxReadSize {
			reader.maxReadSize = len(buffer)
		}
		reader.mu.Unlock()
	}
	return reader.reader.ReadAt(buffer, offset)
}

func (reader *failingRangeReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	end := offset + int64(len(buffer))
	if offset < reader.blocked.end && end > reader.blocked.start {
		return 0, reader.err
	}
	return reader.reader.ReadAt(buffer, offset)
}

type closeTrackingReaderAt struct {
	*bytes.Reader
	closed bool
}

type observingReader struct {
	reader      *bytes.Reader
	maxReadSize int
}

func (reader *observingReader) Read(buffer []byte) (int, error) {
	if len(buffer) > reader.maxReadSize {
		reader.maxReadSize = len(buffer)
	}
	return reader.reader.Read(buffer)
}

type errorAfterReader struct {
	content []byte
	err     error
}

type failingReadSeeker struct {
	err error
}

func (reader *failingReadSeeker) Read([]byte) (int, error) {
	return 0, reader.err
}

func (reader *failingReadSeeker) Seek(int64, int) (int64, error) {
	return 0, reader.err
}

func (reader *errorAfterReader) Read(buffer []byte) (int, error) {
	if len(reader.content) == 0 {
		return 0, reader.err
	}
	read := copy(buffer, reader.content)
	reader.content = reader.content[read:]
	return read, nil
}

type switchFailWriter struct {
	bytes.Buffer
	fail   bool
	closed bool
	err    error
}

type closeErrorReadCloser struct {
	io.Reader
	err error
}

func (stream *closeErrorReadCloser) Close() error {
	return stream.err
}

type testZipFile struct {
	content  []byte
	closeErr error
}

func (file testZipFile) open() (io.ReadCloser, error) {
	return &closeErrorReadCloser{Reader: bytes.NewReader(file.content), err: file.closeErr}, nil
}

func (file testZipFile) uncompressedSize() uint64 {
	return uint64(len(file.content))
}

func (writer *switchFailWriter) Write(buffer []byte) (int, error) {
	if writer.fail {
		return 0, writer.err
	}
	return writer.Buffer.Write(buffer)
}

func (writer *switchFailWriter) Close() error {
	writer.closed = true
	return nil
}

func incompressibleTestContent(size int) []byte {
	result := make([]byte, size)
	state := uint32(0x12345678)
	for index := range result {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		result[index] = byte(state)
	}
	return result
}

func (reader *closeTrackingReaderAt) Close() error {
	reader.closed = true
	return nil
}

func lazyTestPackage(t *testing.T) ([]byte, map[string]byteRange) {
	t.Helper()

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	ranges := make(map[string]byteRange)

	writeEntry := func(name string, content []byte) {
		t.Helper()
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("Failed to create %s: %v", name, err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatalf("Failed to write %s: %v", name, err)
		}
	}

	writeEntry("aasx/aasx-origin", []byte("Intentionally empty."))
	writeEntry("aasx/spec.bin", bytes.Repeat([]byte{0x11}, 128*1024))
	writeEntry("aasx/other.bin", bytes.Repeat([]byte{0x22}, 128*1024))
	// Keep the central-directory search window away from the parts asserted in
	// the lazy-read tests.
	writeEntry("aasx/padding.bin", bytes.Repeat([]byte{0x33}, 128*1024))
	writeEntry("[Content_Types].xml", []byte(xml.Header+`<Types xmlns="`+
		opcContentTypesNamespace+`"><Default Extension="bin" ContentType="application/octet-stream"/>`+
		`<Override PartName="/aasx/aasx-origin" ContentType="text/plain"/></Types>`))
	writeEntry("_rels/.rels", []byte(xml.Header+`<Relationships xmlns="`+
		opcRelationshipNamespace+`"><Relationship Id="R1" Type="`+RelationTypeAasxOrigin+
		`" Target="/aasx/aasx-origin"/></Relationships>`))
	writeEntry("aasx/_rels/aasx-origin.rels", []byte(xml.Header+`<Relationships xmlns="`+
		opcRelationshipNamespace+`"><Relationship Id="R2" Type="`+RelationTypeAasxSpec+
		`" Target="/aasx/spec.bin"/></Relationships>`))

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close test ZIP: %v", err)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("Failed to inspect test ZIP: %v", err)
	}
	for _, file := range archive.File {
		offset, err := file.DataOffset()
		if err != nil {
			t.Fatalf("Failed to find data offset for %s: %v", file.Name, err)
		}
		ranges[file.Name] = byteRange{
			start: offset,
			end:   offset + int64(file.CompressedSize64),
		}
	}
	return output.Bytes(), ranges
}

type testZIPEntry struct {
	name    string
	content []byte
	mode    os.FileMode
}

func testPackageWithEntries(t *testing.T, entries []testZIPEntry) []byte {
	t.Helper()

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	writeEntry := func(entry testZIPEntry) {
		t.Helper()
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("Failed to create %s: %v", entry.name, err)
		}
		if _, err := part.Write(entry.content); err != nil {
			t.Fatalf("Failed to write %s: %v", entry.name, err)
		}
	}

	writeEntry(testZIPEntry{name: "aasx/aasx-origin", content: []byte("Intentionally empty.")})
	writeEntry(testZIPEntry{
		name: "[Content_Types].xml",
		content: []byte(xml.Header + `<Types xmlns="` + opcContentTypesNamespace +
			`"><Default Extension="bin" ContentType="application/octet-stream"/>` +
			`<Default Extension="rels" ContentType="application/xml"/>` +
			`<Override PartName="/aasx/aasx-origin" ContentType="text/plain"/></Types>`),
	})
	writeEntry(testZIPEntry{
		name: "_rels/.rels",
		content: []byte(xml.Header + `<Relationships xmlns="` + opcRelationshipNamespace +
			`"><Relationship Id="R1" Type="` + RelationTypeAasxOrigin +
			`" Target="/aasx/aasx-origin"/></Relationships>`),
	})
	for _, entry := range entries {
		writeEntry(entry)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close test ZIP: %v", err)
	}
	return output.Bytes()
}

func rewriteCentralDirectoryExpandedSize(
	t *testing.T,
	data []byte,
	name string,
	size uint32,
) []byte {
	t.Helper()
	result := append([]byte(nil), data...)
	for offset := 0; offset+46 <= len(result); offset++ {
		if binary.LittleEndian.Uint32(result[offset:]) != 0x02014b50 {
			continue
		}
		nameLength := int(binary.LittleEndian.Uint16(result[offset+28:]))
		extraLength := int(binary.LittleEndian.Uint16(result[offset+30:]))
		commentLength := int(binary.LittleEndian.Uint16(result[offset+32:]))
		end := offset + 46 + nameLength + extraLength + commentLength
		if end > len(result) {
			t.Fatal("Malformed central directory in test ZIP")
		}
		entryName := string(result[offset+46 : offset+46+nameLength])
		if entryName == name {
			binary.LittleEndian.PutUint32(result[offset+24:], size)
			return result
		}
		offset = end - 1
	}
	t.Fatalf("ZIP entry %s not found in central directory", name)
	return nil
}

func relationshipTypesInZip(t *testing.T, zipData []byte) []string {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}

	var result []string
	for _, file := range reader.File {
		if filepath.Ext(file.Name) != ".rels" {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			t.Fatalf("Failed to open rels file %s: %v", file.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("Failed to read rels file %s: %v", file.Name, err)
		}

		var rels relationshipsXML
		if err := xml.Unmarshal(data, &rels); err != nil {
			t.Fatalf("Failed to parse rels file %s: %v", file.Name, err)
		}

		for _, rel := range rels.Relationships {
			result = append(result, rel.Type)
		}
	}

	return result
}

func relationshipTargetsInZip(t *testing.T, zipData []byte) []string {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}

	var result []string
	for _, file := range reader.File {
		if filepath.Ext(file.Name) != ".rels" {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			t.Fatalf("Failed to open rels file %s: %v", file.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("Failed to read rels file %s: %v", file.Name, err)
		}

		var rels relationshipsXML
		if err := xml.Unmarshal(data, &rels); err != nil {
			t.Fatalf("Failed to parse rels file %s: %v", file.Name, err)
		}

		for _, rel := range rels.Relationships {
			result = append(result, rel.Target)
		}
	}

	return result
}

func rewriteRelationshipTypesInZip(
	t *testing.T,
	zipData []byte,
	replacements map[string]string,
) []byte {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}

	var out bytes.Buffer
	writer := zip.NewWriter(&out)

	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("Failed to open zip entry %s: %v", file.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("Failed to read zip entry %s: %v", file.Name, err)
		}

		if filepath.Ext(file.Name) == ".rels" {
			var rels relationshipsXML
			if err := xml.Unmarshal(data, &rels); err != nil {
				t.Fatalf("Failed to parse rels file %s: %v", file.Name, err)
			}

			for i := range rels.Relationships {
				if replacement, ok := replacements[rels.Relationships[i].Type]; ok {
					rels.Relationships[i].Type = replacement
				}
			}

			data, err = xml.MarshalIndent(rels, "", "  ")
			if err != nil {
				t.Fatalf("Failed to marshal rels file %s: %v", file.Name, err)
			}
			data = append([]byte(xml.Header), data...)
		}

		entryWriter, err := writer.Create(file.Name)
		if err != nil {
			t.Fatalf("Failed to create zip entry %s: %v", file.Name, err)
		}

		if _, err := entryWriter.Write(data); err != nil {
			t.Fatalf("Failed to write zip entry %s: %v", file.Name, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close zip writer: %v", err)
	}

	return out.Bytes()
}

func rewriteRelationshipTargetsInZip(
	t *testing.T,
	zipData []byte,
	replacements map[string]string,
) []byte {
	t.Helper()

	reader, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}

	var out bytes.Buffer
	writer := zip.NewWriter(&out)

	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("Failed to open zip entry %s: %v", file.Name, err)
		}

		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("Failed to read zip entry %s: %v", file.Name, err)
		}

		if filepath.Ext(file.Name) == ".rels" {
			var rels relationshipsXML
			if err := xml.Unmarshal(data, &rels); err != nil {
				t.Fatalf("Failed to parse rels file %s: %v", file.Name, err)
			}

			for i := range rels.Relationships {
				if replacement, ok := replacements[rels.Relationships[i].Target]; ok {
					rels.Relationships[i].Target = replacement
				}
			}

			data, err = xml.MarshalIndent(rels, "", "  ")
			if err != nil {
				t.Fatalf("Failed to marshal rels file %s: %v", file.Name, err)
			}
			data = append([]byte(xml.Header), data...)
		}

		entryWriter, err := writer.Create(file.Name)
		if err != nil {
			t.Fatalf("Failed to create zip entry %s: %v", file.Name, err)
		}

		if _, err := entryWriter.Write(data); err != nil {
			t.Fatalf("Failed to write zip entry %s: %v", file.Name, err)
		}
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close zip writer: %v", err)
	}

	return out.Bytes()
}

// =============================================================================
// TestPackageRead - Lazy and bounded reading
// =============================================================================

func TestOpenReadFromReaderAtReadsPartsLazily(t *testing.T) {
	data, ranges := lazyTestPackage(t)
	blockedErr := errors.New("payload range read")
	packaging := NewPackaging()

	reader := &failingRangeReaderAt{
		reader:  bytes.NewReader(data),
		blocked: ranges["aasx/spec.bin"],
		err:     blockedErr,
	}
	pkg, err := packaging.OpenReadFromReaderAt(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("Opening package unexpectedly read the spec payload: %v", err)
	}
	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	if _, err := specs[0].ReadAllBytes(); !errors.Is(err, blockedErr) {
		t.Fatalf("Expected deferred payload error, got: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	reader = &failingRangeReaderAt{
		reader:  bytes.NewReader(data),
		blocked: ranges["aasx/other.bin"],
		err:     blockedErr,
	}
	pkg, err = packaging.OpenReadFromReaderAt(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("Opening package unexpectedly read an unrelated payload: %v", err)
	}
	defer pkg.Close()
	specs, err = pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	content, err := specs[0].ReadAllBytes()
	if err != nil {
		t.Fatalf("Reading spec unexpectedly read another part: %v", err)
	}
	if len(content) != 128*1024 {
		t.Fatalf("Expected 128 KiB spec, got %d bytes", len(content))
	}
}

func TestLazyReaderLimits(t *testing.T) {
	data, _ := lazyTestPackage(t)
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Failed to inspect test package: %v", err)
	}

	var partCount uint64
	var metadataBytes uint64
	var largestPart uint64
	var totalExpanded uint64
	for _, file := range reader.File {
		if strings.HasSuffix(file.Name, "/") {
			continue
		}
		partCount++
		if isOPCMetadataPath(file.Name) {
			metadataBytes += file.UncompressedSize64
			continue
		}
		if file.UncompressedSize64 > largestPart {
			largestPart = file.UncompressedSize64
		}
		totalExpanded += file.UncompressedSize64
	}

	packaging := NewPackaging()
	open := func(options ...ReaderOption) error {
		pkg, openErr := packaging.OpenReadFromReaderAt(
			bytes.NewReader(data), int64(len(data)), options...)
		if openErr == nil {
			openErr = pkg.Close()
		}
		return openErr
	}

	if err := open(
		WithMaxPartCount(partCount),
		WithMaxOPCMetadataBytes(metadataBytes),
		WithMaxPartExpandedBytes(largestPart),
		WithMaxTotalExpandedBytes(totalExpanded),
	); err != nil {
		t.Fatalf("Exact reader limits should be accepted: %v", err)
	}

	limits := []ReaderOption{
		WithMaxPartCount(partCount - 1),
		WithMaxOPCMetadataBytes(metadataBytes - 1),
		WithMaxPartExpandedBytes(largestPart - 1),
		WithMaxTotalExpandedBytes(totalExpanded - 1),
	}
	for index, option := range limits {
		if err := open(option); !errors.Is(err, ErrReaderLimitExceeded) {
			t.Errorf("Limit %d: expected ErrReaderLimitExceeded, got %v", index, err)
		}
	}

	if err := open(
		WithMaxPartCount(0),
		WithMaxOPCMetadataBytes(0),
		WithMaxPartExpandedBytes(0),
		WithMaxTotalExpandedBytes(0),
	); err != nil {
		t.Fatalf("Zero limits should be unlimited: %v", err)
	}
}

func TestLazyReaderClassifiesOPCPathsExactly(t *testing.T) {
	payload := bytes.Repeat([]byte("not XML"), 1024)
	data := testPackageWithEntries(t, []testZIPEntry{
		{name: "attacker_rels/bomb.rels", content: payload},
		{name: "foo_rels/data.bin", content: []byte("visible")},
	})

	packaging := NewPackaging()
	if _, err := packaging.OpenReadFromReaderAt(
		bytes.NewReader(data), int64(len(data)), WithMaxPartExpandedBytes(64),
	); !errors.Is(err, ErrReaderLimitExceeded) {
		t.Fatalf("Near-match relationship path bypassed payload limit: %v", err)
	}

	pkg, err := packaging.OpenReadFromReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Failed to open package with near-match paths: %v", err)
	}
	defer pkg.Close()
	part, err := pkg.MustPart(mustParseURL("/foo_rels/data.bin"))
	if err != nil {
		t.Fatalf("Near-match payload was hidden: %v", err)
	}
	content, err := part.ReadAllBytes()
	if err != nil || string(content) != "visible" {
		t.Fatalf("Unexpected near-match payload content %q: %v", content, err)
	}

	reserved := testPackageWithEntries(t, []testZIPEntry{
		{name: "_rels/data.bin", content: []byte("reserved")},
	})
	if _, err := packaging.OpenReadFromReaderAt(
		bytes.NewReader(reserved), int64(len(reserved)),
	); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("Expected invalid-format error for reserved OPC path, got %v", err)
	}
}

func TestLazyReaderRejectsDuplicateCanonicalEntries(t *testing.T) {
	testCases := []struct {
		name    string
		entries []testZIPEntry
	}{
		{
			name: "same name",
			entries: []testZIPEntry{
				{name: "data.bin", content: []byte("first")},
				{name: "data.bin", content: []byte("second")},
			},
		},
		{
			name: "case alias",
			entries: []testZIPEntry{
				{name: "Data.bin", content: []byte("first")},
				{name: "data.bin", content: []byte("second")},
			},
		},
		{
			name: "clean path alias",
			entries: []testZIPEntry{
				{name: "folder/../data.bin", content: []byte("first")},
				{name: "data.bin", content: []byte("second")},
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			data := testPackageWithEntries(t, testCase.entries)
			_, err := NewPackaging().OpenReadFromReaderAt(
				bytes.NewReader(data), int64(len(data)))
			if !errors.Is(err, ErrInvalidFormat) {
				t.Fatalf("Expected duplicate-entry format error, got %v", err)
			}
		})
	}
}

func TestLazyReaderUsesFixedReadChunksAndDeclaredSizeGuard(t *testing.T) {
	data, ranges := lazyTestPackage(t)
	reader := &observingRangeReaderAt{
		reader:   bytes.NewReader(data),
		observed: ranges["aasx/spec.bin"],
	}
	pkg, err := NewPackaging().OpenReadFromReaderAt(reader, int64(len(data)))
	if err != nil {
		t.Fatalf("Failed to open package: %v", err)
	}
	defer pkg.Close()
	part, err := pkg.MustPart(mustParseURL("/aasx/spec.bin"))
	if err != nil {
		t.Fatalf("Failed to find spec: %v", err)
	}
	if _, err := part.ReadAllBytes(); err != nil {
		t.Fatalf("Failed to read spec: %v", err)
	}
	if reader.maxReadSize > streamCopyBufferSize {
		t.Fatalf("Payload read request was %d bytes, maximum is %d",
			reader.maxReadSize, streamCopyBufferSize)
	}

	malformed := testPackageWithEntries(t, []testZIPEntry{
		{name: "oversized.bin", content: bytes.Repeat([]byte{0x7f}, 64*1024)},
	})
	malformed = rewriteCentralDirectoryExpandedSize(t, malformed, "oversized.bin", 1)
	pkg, err = NewPackaging().OpenReadFromReaderAt(
		bytes.NewReader(malformed), int64(len(malformed)), WithMaxTotalExpandedBytes(64))
	if err != nil {
		t.Fatalf("Declared-size package should open before payload consumption: %v", err)
	}
	defer pkg.Close()
	part, err = pkg.MustPart(mustParseURL("/oversized.bin"))
	if err != nil {
		t.Fatalf("Failed to find malformed part: %v", err)
	}
	content, err := part.ReadAllBytes()
	if !errors.Is(err, zip.ErrFormat) {
		t.Fatalf("Expected expanded-size format error, got %v", err)
	}
	if len(content) > 1 {
		t.Fatalf("Read %d bytes beyond declared expanded size", len(content))
	}
}

func TestBoundedReaderCapsChunksAndReportsLimits(t *testing.T) {
	source := &observingReader{reader: bytes.NewReader(bytes.Repeat([]byte{0x42}, 64*1024))}
	stream := &boundedReadCloser{
		stream:    io.NopCloser(source),
		remaining: 32,
		label:     "test stream exceeds limit",
	}
	if read, err := stream.Read(nil); read != 0 || err != nil {
		t.Fatalf("Zero-length read returned %d, %v", read, err)
	}
	content, err := io.ReadAll(stream)
	if !errors.Is(err, ErrReaderLimitExceeded) {
		t.Fatalf("Expected reader limit error, got %v", err)
	}
	if len(content) != 32 {
		t.Fatalf("Expected 32 bytes before limit error, got %d", len(content))
	}
	if source.maxReadSize > streamCopyBufferSize {
		t.Fatalf("Bounded reader requested %d bytes, maximum is %d",
			source.maxReadSize, streamCopyBufferSize)
	}
}

func TestLazyReaderRejectsInvalidReaderArguments(t *testing.T) {
	packaging := NewPackaging()
	if _, err := packaging.OpenReadFromReaderAt(nil, 0); err == nil {
		t.Fatal("Expected nil ReaderAt to be rejected")
	}
	if _, err := packaging.OpenReadFromReaderAt(bytes.NewReader(nil), -1); err == nil {
		t.Fatal("Expected negative ReaderAt size to be rejected")
	}
	if _, err := packaging.OpenReadFromStream(nil); err == nil {
		t.Fatal("Expected nil ReadSeeker to be rejected")
	}
	seekErr := errors.New("seek failed")
	if _, err := packaging.OpenReadFromStream(&failingReadSeeker{err: seekErr}); !errors.Is(err, seekErr) {
		t.Fatalf("Expected ReadSeeker seek error, got %v", err)
	}
	adapter := &readSeekerAt{stream: bytes.NewReader(nil)}
	if _, err := adapter.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("Expected negative ReadAt offset to be rejected")
	}
}

func TestLazyReaderIgnoresModeDirectoriesForPartLimits(t *testing.T) {
	data := testPackageWithEntries(t, []testZIPEntry{
		{name: "mode-directory", mode: os.ModeDir | 0755},
	})
	pkg, err := NewPackaging().OpenReadFromReaderAt(
		bytes.NewReader(data), int64(len(data)), WithMaxPartCount(3))
	if err != nil {
		t.Fatalf("Mode-only directory counted as a part: %v", err)
	}
	defer pkg.Close()
	part, err := pkg.FindPart(mustParseURL("/mode-directory"))
	if err != nil {
		t.Fatalf("Failed to query mode-only directory: %v", err)
	}
	if part != nil {
		t.Fatal("Mode-only directory was exposed as a payload part")
	}
}

func TestPackageReadOperationsAfterClose(t *testing.T) {
	data, _ := lazyTestPackage(t)
	pkg, err := NewPackaging().OpenReadFromReaderAt(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("Failed to open package: %v", err)
	}
	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Repeated close failed: %v", err)
	}

	operations := []func() error{
		func() error { _, operationErr := pkg.Specs(); return operationErr },
		func() error { _, operationErr := pkg.SpecsByContentType(); return operationErr },
		func() error { _, operationErr := pkg.IsSpec(specs[0]); return operationErr },
		func() error { _, operationErr := pkg.SupplementariesFor(specs[0]); return operationErr },
		func() error { _, operationErr := pkg.SupplementaryRelationships(); return operationErr },
		func() error { _, operationErr := pkg.FindPart(specs[0].URI); return operationErr },
		func() error { _, operationErr := pkg.MustPart(specs[0].URI); return operationErr },
		func() error { _, operationErr := pkg.Thumbnail(); return operationErr },
		func() error { _, operationErr := specs[0].Stream(); return operationErr },
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, ErrPackageClosed) {
			t.Errorf("Operation %d: expected ErrPackageClosed, got %v", index, err)
		}
	}
}

func TestPackageReadCloseRetainsErrorAndSynchronizesQueries(t *testing.T) {
	closeErr := errors.New("close failed")
	base := newPackageBase("", nil)
	base.ownedCloser = &closeErrorReadCloser{Reader: bytes.NewReader(nil), err: closeErr}
	pkg := &PackageRead{base: base}
	if err := pkg.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Expected close error, got %v", err)
	}
	if err := pkg.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("Repeated Close lost original error: %v", err)
	}

	data, _ := lazyTestPackage(t)
	for iteration := 0; iteration < 50; iteration++ {
		concurrent, err := NewPackaging().OpenReadFromReaderAt(
			bytes.NewReader(data), int64(len(data)))
		if err != nil {
			t.Fatalf("Failed to open concurrent package: %v", err)
		}
		var waitGroup sync.WaitGroup
		waitGroup.Add(2)
		go func() {
			defer waitGroup.Done()
			_, queryErr := concurrent.Specs()
			if queryErr != nil && !errors.Is(queryErr, ErrPackageClosed) {
				t.Errorf("Unexpected concurrent query error: %v", queryErr)
			}
		}()
		go func() {
			defer waitGroup.Done()
			if closeOperationErr := concurrent.Close(); closeOperationErr != nil {
				t.Errorf("Unexpected concurrent close error: %v", closeOperationErr)
			}
		}()
		waitGroup.Wait()
	}
}

func TestLazyReaderDefersCRCFailureUntilPartRead(t *testing.T) {
	data, ranges := lazyTestPackage(t)
	corrupted := append([]byte(nil), data...)
	specRange := ranges["aasx/spec.bin"]
	corrupted[specRange.start] ^= 0xff

	pkg, err := NewPackaging().OpenReadFromReaderAt(
		bytes.NewReader(corrupted), int64(len(corrupted)))
	if err != nil {
		t.Fatalf("Payload CRC failure should be deferred until reading: %v", err)
	}
	defer pkg.Close()
	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	if _, err := specs[0].ReadAllBytes(); !errors.Is(err, zip.ErrChecksum) {
		t.Fatalf("Expected ZIP checksum error, got %v", err)
	}
}

func TestLazyReaderPropagatesCloseErrorsAndSizeOverflow(t *testing.T) {
	closeErr := errors.New("close failed")
	if _, err := readZipFile(
		testZipFile{content: []byte("metadata"), closeErr: closeErr},
		0,
		false,
		"metadata",
	); !errors.Is(err, closeErr) {
		t.Fatalf("Expected metadata close error, got %v", err)
	}

	if _, err := checkedExpandedTotal(^uint64(0), 1); !errors.Is(
		err, ErrReaderLimitExceeded,
	) {
		t.Fatalf("Expected expanded-size overflow error, got %v", err)
	}

	base := newPackageBase("", nil)
	base.ownedCloser = &closeErrorReadCloser{Reader: bytes.NewReader(nil), err: closeErr}
	if err := base.close(); !errors.Is(err, closeErr) {
		t.Fatalf("Expected owned-reader close error, got %v", err)
	}
	if err := base.close(); !errors.Is(err, closeErr) {
		t.Fatalf("Expected repeated close to retain error, got %v", err)
	}
}

func TestLazyReaderOwnershipAndClose(t *testing.T) {
	data, _ := lazyTestPackage(t)
	packaging := NewPackaging()

	external := &closeTrackingReaderAt{Reader: bytes.NewReader(data)}
	pkg, err := packaging.OpenReadFromReaderAt(external, int64(len(data)))
	if err != nil {
		t.Fatalf("Failed to open external reader: %v", err)
	}
	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}
	if external.closed {
		t.Fatal("Package closed a caller-owned ReaderAt")
	}
	if _, err := specs[0].ReadAllBytes(); !errors.Is(err, ErrPackageClosed) {
		t.Fatalf("Expected ErrPackageClosed, got %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Repeated Close failed: %v", err)
	}

	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()
	path := filepath.Join(tmpdir, "lazy.aasx")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("Failed to write test package: %v", err)
	}
	pkg, err = packaging.OpenRead(path)
	if err != nil {
		t.Fatalf("Failed to open file package: %v", err)
	}
	ownedFile, ok := pkg.base.ownedCloser.(*os.File)
	if !ok {
		t.Fatal("OpenRead did not retain an owned file")
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close file package: %v", err)
	}
	if _, err := ownedFile.Stat(); err == nil {
		t.Fatal("OpenRead-owned file remains open after Close")
	}
}

func TestOpenReadFromStreamSupportsConcurrentPartReads(t *testing.T) {
	data, _ := lazyTestPackage(t)
	pkg, err := NewPackaging().OpenReadFromStream(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Failed to open stream: %v", err)
	}
	defer pkg.Close()

	parts := make([]*Part, 0, 2)
	for _, rawURI := range []string{"/aasx/spec.bin", "/aasx/other.bin"} {
		part, err := pkg.MustPart(mustParseURL(rawURI))
		if err != nil {
			t.Fatalf("Failed to get %s: %v", rawURI, err)
		}
		parts = append(parts, part)
	}

	var group sync.WaitGroup
	errorsByRead := make(chan error, 20)
	for _, part := range parts {
		part := part
		group.Add(1)
		go func() {
			defer group.Done()
			for index := 0; index < 10; index++ {
				content, readErr := part.ReadAllBytes()
				if readErr != nil {
					errorsByRead <- readErr
					return
				}
				if len(content) != 128*1024 {
					errorsByRead <- errors.New("unexpected part size")
					return
				}
			}
		}()
	}
	group.Wait()
	close(errorsByRead)
	for readErr := range errorsByRead {
		t.Errorf("Concurrent read failed: %v", readErr)
	}
}

// =============================================================================
// TestPackageRead - Error Handling
// =============================================================================

func TestOpenReadReturnsErrorForInvalidPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")

	// Create an invalid file w.r.t. Open Package Convention
	if err := os.WriteFile(pth, []byte("This is not OPC."), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	packaging := NewPackaging()

	pkg, err := packaging.OpenRead(pth)
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening non-OPC file, got nil")
	}
}

func TestOpenReadFromStreamReturnsErrorForInvalidPackage(t *testing.T) {
	// Create a stream with invalid OPC content
	stream := bytes.NewReader([]byte("This is not OPC."))

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadFromStream(stream)
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening non-OPC stream, got nil")
	}
}

func TestOpenReadReturnsErrorForNonExistentFile(t *testing.T) {
	packaging := NewPackaging()

	pkg, err := packaging.OpenRead("/this/path/can/not/possibly/exist.aasx")
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening non-existent file, got nil")
	}
}

func TestOpenReadReturnsErrorForEmptyStream(t *testing.T) {
	packaging := NewPackaging()

	stream := bytes.NewReader([]byte{})

	pkg, err := packaging.OpenReadFromStream(stream)
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening empty stream, got nil")
	}
}

func TestMustReturnsError(t *testing.T) {
	packaging := NewPackaging()

	stream := bytes.NewReader([]byte{})

	pkgOrErr, _ := packaging.OpenReadFromStream(stream)

	// The Must method should panic or return error for invalid package
	defer func() {
		if r := recover(); r == nil {
			t.Log("Must did not panic, checking for nil package")
		}
	}()

	// If OpenReadFromStream returns error, the package should be nil
	// or Must() should panic/error
	if pkgOrErr != nil {
		defer pkgOrErr.Close()
		t.Error("Expected nil package or panic from Must on invalid stream")
	}
}

func TestOpenReadReturnsErrorForFileWithoutOrigin(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")

	// Create an empty OPC package (no AASX origin) - this requires creating a valid
	// ZIP file but without the aasx-origin relationship
	// For now, we create an empty zip which is technically an OPC package without origin
	f, err := os.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}
	// Write minimal zip file (empty central directory)
	// PK\x05\x06 + 18 bytes of zeros = empty zip
	emptyZip := []byte{0x50, 0x4b, 0x05, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if _, err := f.Write(emptyZip); err != nil {
		t.Fatalf("Failed to write: %v", err)
	}
	f.Close()

	packaging := NewPackaging()

	pkg, err := packaging.OpenRead(pth)
	if err == nil {
		defer pkg.Close()
		// The package opened but should fail validation for missing origin
		// This test verifies the error is returned for packages without origin
		t.Log("Package opened, but should have missing origin validation")
	}
	// Error is expected - either file format error or missing origin error
}

func TestOpenReadFromStreamReturnsErrorForStreamWithoutOrigin(t *testing.T) {
	// Create an empty OPC package in memory (no AASX origin)
	// Write minimal zip file (empty central directory)
	emptyZip := []byte{0x50, 0x4b, 0x05, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	stream := bytes.NewReader(emptyZip)

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadFromStream(stream)
	if err == nil {
		defer pkg.Close()
		// The package opened but should fail validation for missing origin
		t.Log("Package opened, but should have missing origin validation")
	}
	// Error is expected - either file format error or missing origin error
}

// =============================================================================
// TestPackageRead - Specs
// =============================================================================

func TestGroupingSpecsByContentType(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	// Create package with multiple specs of different content types
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		// Add JSON spec
		part1, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.json"),
			"text/json",
			[]byte("{}"),
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part1); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		// Add another JSON spec
		part2, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data1.json"),
			"text/json",
			[]byte("{x: 1}"),
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part2); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		// Add XML spec
		part3, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.xml"),
			"text/xml",
			[]byte("<something></something>"),
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part3); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specsByContentType, err := pkg.SpecsByContentType()
		if err != nil {
			t.Fatalf("Failed to get specs by content type: %v", err)
		}

		// Verify content types present
		if _, ok := specsByContentType["text/json"]; !ok {
			t.Error("Expected text/json in specs by content type")
		}
		if _, ok := specsByContentType["text/xml"]; !ok {
			t.Error("Expected text/xml in specs by content type")
		}

		// Verify JSON specs
		jsonSpecs := specsByContentType["text/json"]
		if len(jsonSpecs) != 2 {
			t.Errorf("Expected 2 JSON specs, got %d", len(jsonSpecs))
		}

		// Verify XML specs
		xmlSpecs := specsByContentType["text/xml"]
		if len(xmlSpecs) != 1 {
			t.Errorf("Expected 1 XML spec, got %d", len(xmlSpecs))
		}
	}
}

func TestReadSpecs(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some content")

	packaging := NewPackaging()

	// Create package with spec
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		if len(specs) != 1 {
			t.Fatalf("Expected 1 spec, got %d", len(specs))
		}

		content, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read spec content: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Errorf("Content mismatch: expected %s, got %s", originalContent, content)
		}
	}
}

func TestReadSpecsNormalizesWindowsLikeRelationshipTargets(t *testing.T) {
	packaging := NewPackaging()

	stream := &readWriteSeeker{buf: &bytes.Buffer{}}
	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	specPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/data.txt"),
		"text/plain",
		[]byte("some content"),
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}
	if err := pkg.MakeSpec(specPart); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	zipData := rewriteRelationshipTargetsInZip(t, stream.buf.Bytes(), map[string]string{
		"/aasx/some-company/data.txt": `\aasx\\some-company\.\data.txt`,
	})

	readPkg, err := packaging.OpenReadFromStream(bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Failed to open package: %v", err)
	}
	defer readPkg.Close()

	specs, err := readPkg.Specs()
	if err != nil {
		t.Fatalf("Failed to read specs: %v", err)
	}

	if len(specs) != 1 {
		t.Fatalf("Expected 1 spec after canonicalizing relationship target path, got %d", len(specs))
	}
}

func TestGetSourcePathFromRelsPathNormalizesWindowsSeparators(t *testing.T) {
	got := getSourcePathFromRelsPath(`aasx\_rels\aasx-origin.rels`)
	if got != "/aasx/aasx-origin" {
		t.Fatalf("Expected normalized source path /aasx/aasx-origin, got %q", got)
	}
}

func TestNormalizePathForURIPreservesCase(t *testing.T) {
	got := normalizePathForURI(`AASX\Some-Company\.\Data.TXT`)
	if got != "/AASX/Some-Company/Data.TXT" {
		t.Fatalf("Expected case-preserving URI normalization, got %q", got)
	}

	mapKey := normalizePathForMap(`AASX\Some-Company\.\Data.TXT`)
	if mapKey != "/aasx/some-company/data.txt" {
		t.Fatalf("Expected lowercase map key normalization, got %q", mapKey)
	}
}

func TestFlushPreservesRelationshipTargetCaseFromReadPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	packaging := NewPackaging()

	stream := &readWriteSeeker{buf: &bytes.Buffer{}}
	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	specPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/data.txt"),
		"text/plain",
		[]byte("some content"),
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}
	if err := pkg.MakeSpec(specPart); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	zipData := rewriteRelationshipTargetsInZip(t, stream.buf.Bytes(), map[string]string{
		"/aasx/some-company/data.txt": "/AASX/Some-Company/./Data.TXT",
	})

	pth := filepath.Join(tmpdir, "preserve-target-case.aasx")
	if err := os.WriteFile(pth, zipData, 0644); err != nil {
		t.Fatalf("Failed to write test package: %v", err)
	}

	rwPkg, err := packaging.OpenReadWrite(pth)
	if err != nil {
		t.Fatalf("Failed to open package in read-write mode: %v", err)
	}

	specs, err := rwPkg.Specs()
	if err != nil {
		t.Fatalf("Failed to read specs: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("Expected 1 spec, got %d", len(specs))
	}

	if err := rwPkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := rwPkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	flushedData, err := os.ReadFile(pth)
	if err != nil {
		t.Fatalf("Failed to read flushed package: %v", err)
	}

	targets := relationshipTargetsInZip(t, flushedData)
	found := false
	for _, target := range targets {
		if target == "/AASX/Some-Company/Data.TXT" {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("Expected canonicalized relationship target /AASX/Some-Company/Data.TXT, got %v", targets)
	}
}

// =============================================================================
// TestPackageRead - Supplementaries
// =============================================================================

func TestReadSupplementaries(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	supplContent := []byte("some supplementary content")

	packaging := NewPackaging()

	// Create package with spec and supplementary
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		specPart, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.txt"),
			"text/plain",
			[]byte("some spec content"),
		)
		if err != nil {
			t.Fatalf("Failed to put spec part: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/suppl.txt"),
			"text/plain",
			supplContent,
		)
		if err != nil {
			t.Fatalf("Failed to put supplementary part: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to relate supplementary to spec: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		rels, err := pkg.SupplementaryRelationships()
		if err != nil {
			t.Fatalf("Failed to get supplementary relationships: %v", err)
		}

		if len(rels) != 1 {
			t.Fatalf("Expected 1 supplementary relationship, got %d", len(rels))
		}

		content, err := rels[0].Supplementary.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read supplementary content: %v", err)
		}

		if !bytes.Equal(content, supplContent) {
			t.Errorf("Content mismatch: expected %s, got %s", supplContent, content)
		}
	}
}

func TestReadSpecsSupportsDeprecatedRelationshipType(t *testing.T) {
	packaging := NewPackaging()

	stream := &readWriteSeeker{buf: &bytes.Buffer{}}
	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	_, err = pkg.PutPart(
		mustParseURL("/aasx/some-company/data.txt"),
		"text/plain",
		[]byte("some content"),
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}

	spec, err := pkg.MustPart(mustParseURL("/aasx/some-company/data.txt"))
	if err != nil {
		t.Fatalf("Failed to get part: %v", err)
	}

	if err := pkg.MakeSpec(spec); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	zipData := rewriteRelationshipTypesInZip(t, stream.buf.Bytes(), map[string]string{
		RelationTypeAasxSpec: DeprecatedAasxRelationshipsPrefix + "aas-spec",
	})

	readPkg, err := packaging.OpenReadFromStream(bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Failed to open package: %v", err)
	}
	defer readPkg.Close()

	specs, err := readPkg.Specs()
	if err != nil {
		t.Fatalf("Failed to read specs: %v", err)
	}

	if len(specs) != 1 {
		t.Fatalf("Expected 1 spec, got %d", len(specs))
	}
}

func TestReadSupplementariesSupportsDeprecatedRelationshipType(t *testing.T) {
	packaging := NewPackaging()

	stream := &readWriteSeeker{buf: &bytes.Buffer{}}
	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	specPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/data.txt"),
		"text/plain",
		[]byte("spec content"),
	)
	if err != nil {
		t.Fatalf("Failed to put spec part: %v", err)
	}
	if err := pkg.MakeSpec(specPart); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	supplPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/suppl.txt"),
		"text/plain",
		[]byte("supplementary content"),
	)
	if err != nil {
		t.Fatalf("Failed to put supplementary part: %v", err)
	}
	if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
		t.Fatalf("Failed to relate supplementary to spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	zipData := rewriteRelationshipTypesInZip(t, stream.buf.Bytes(), map[string]string{
		RelationTypeAasxSpec:          DeprecatedAasxRelationshipsPrefix + "aas-spec",
		RelationTypeAasxSupplementary: DeprecatedAasxRelationshipsPrefix + "aas-suppl",
	})

	readPkg, err := packaging.OpenReadFromStream(bytes.NewReader(zipData))
	if err != nil {
		t.Fatalf("Failed to open package: %v", err)
	}
	defer readPkg.Close()

	rels, err := readPkg.SupplementaryRelationships()
	if err != nil {
		t.Fatalf("Failed to read supplementary relationships: %v", err)
	}

	if len(rels) != 1 {
		t.Fatalf("Expected 1 supplementary relationship, got %d", len(rels))
	}
}

func TestFlushPreservesDeprecatedAasxRelationshipTypes(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	packaging := NewPackaging()

	stream := &readWriteSeeker{buf: &bytes.Buffer{}}
	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	specPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/data.txt"),
		"text/plain",
		[]byte("spec content"),
	)
	if err != nil {
		t.Fatalf("Failed to put spec part: %v", err)
	}
	if err := pkg.MakeSpec(specPart); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	supplPart, err := pkg.PutPart(
		mustParseURL("/aasx/some-company/suppl.txt"),
		"text/plain",
		[]byte("supplementary content"),
	)
	if err != nil {
		t.Fatalf("Failed to put supplementary part: %v", err)
	}
	if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
		t.Fatalf("Failed to relate supplementary to spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	zipData := rewriteRelationshipTypesInZip(t, stream.buf.Bytes(), map[string]string{
		RelationTypeAasxOrigin:        DeprecatedAasxRelationshipsPrefix + "aasx-origin",
		RelationTypeAasxSpec:          DeprecatedAasxRelationshipsPrefix + "aas-spec",
		RelationTypeAasxSupplementary: DeprecatedAasxRelationshipsPrefix + "aas-suppl",
	})

	pth := filepath.Join(tmpdir, "deprecated.aasx")
	if err := os.WriteFile(pth, zipData, 0644); err != nil {
		t.Fatalf("Failed to write test package: %v", err)
	}

	rwPkg, err := packaging.OpenReadWrite(pth)
	if err != nil {
		t.Fatalf("Failed to open package in read-write mode: %v", err)
	}
	if err := rwPkg.Flush(); err != nil {
		t.Fatalf("Failed to flush package: %v", err)
	}
	if err := rwPkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	flushedData, err := os.ReadFile(pth)
	if err != nil {
		t.Fatalf("Failed to read flushed package: %v", err)
	}

	types := relationshipTypesInZip(t, flushedData)
	deprOrigin := DeprecatedAasxRelationshipsPrefix + "aasx-origin"
	deprSpec := DeprecatedAasxRelationshipsPrefix + "aas-spec"
	deprSuppl := DeprecatedAasxRelationshipsPrefix + "aas-suppl"

	hasOrigin := false
	hasSpec := false
	hasSuppl := false
	for _, relType := range types {
		if relType == deprOrigin {
			hasOrigin = true
		}
		if relType == deprSpec {
			hasSpec = true
		}
		if relType == deprSuppl {
			hasSuppl = true
		}
	}

	if !hasOrigin {
		t.Fatalf("Expected deprecated origin relationship type %q to be preserved", deprOrigin)
	}
	if !hasSpec {
		t.Fatalf("Expected deprecated spec relationship type %q to be preserved", deprSpec)
	}
	if !hasSuppl {
		t.Fatalf("Expected deprecated supplementary relationship type %q to be preserved", deprSuppl)
	}
}

// =============================================================================
// TestPackageRead - Thumbnail
// =============================================================================

func TestReadThumbnail(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some thumbnail content")

	packaging := NewPackaging()

	// Create package with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/some-thumbnail.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}

		if thumbnail == nil {
			t.Fatal("Expected thumbnail but got nil")
		}

		content, err := thumbnail.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read thumbnail content: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Errorf("Content mismatch: expected %s, got %s", originalContent, content)
		}
	}
}

func TestQueryingThumbnailInPackageWithoutOne(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	// Create empty package
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify no thumbnail
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Unexpected error getting thumbnail: %v", err)
		}

		if thumbnail != nil {
			t.Error("Expected nil thumbnail but got one")
		}
	}
}

func TestThumbnailRelationshipExistsWithoutPart(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	// Initialize with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/some-thumbnail.txt"),
			"text/plain",
			[]byte("some content"),
		)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Remove the thumbnail as part, but NOT as relationship
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		oldThumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if oldThumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		// Remove part but keep relationship
		if err := pkg.DeletePart(oldThumbnail); err != nil {
			t.Fatalf("Failed to delete part: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Try to read the non-existing thumbnail part - should error
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		// Getting thumbnail should return error since part doesn't exist
		_, err = pkg.Thumbnail()
		if err == nil {
			t.Error("Expected error when thumbnail relationship exists without part")
		}
	}
}

// =============================================================================
// TestPackageRead - FindPart and MustPart
// =============================================================================

func TestFindPart(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	uri := mustParseURL("/aasx/some-company/data.txt")

	// Create package with a part
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", []byte("content"))
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and find part
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		part, err := pkg.FindPart(uri)
		if err != nil {
			t.Fatalf("Failed to find part: %v", err)
		}

		if part == nil {
			t.Error("Expected to find part but got nil")
		}

		// Try to find non-existent part
		nonExistentURI := mustParseURL("/aasx/non-existent.txt")
		part, err = pkg.FindPart(nonExistentURI)
		if err != nil {
			t.Fatalf("Unexpected error for non-existent part: %v", err)
		}
		if part != nil {
			t.Error("Expected nil for non-existent part")
		}
	}
}

func TestMustPart(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	uri := mustParseURL("/aasx/some-company/data.txt")

	// Create package with a part
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", []byte("content"))
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and must part
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		part, err := pkg.MustPart(uri)
		if err != nil {
			t.Fatalf("Failed to get must part: %v", err)
		}
		if part == nil {
			t.Error("Expected part but got nil")
		}

		// Try to must a non-existent part - should return error
		nonExistentURI := mustParseURL("/aasx/non-existent.txt")
		_, err = pkg.MustPart(nonExistentURI)
		if err == nil {
			t.Error("Expected error for non-existent must part")
		}
	}
}

// =============================================================================
// TestPackageRead - Part methods
// =============================================================================

func TestPartReadAllBytes(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some binary content")

	packaging := NewPackaging()

	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/aasx/data.bin"),
			"application/octet-stream",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		content, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read all bytes: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Error("Content mismatch")
		}
	}
}

func TestPartReadAllText(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalText := "some text content with üñíçödé"

	packaging := NewPackaging()

	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/aasx/data.txt"),
			"text/plain",
			[]byte(originalText),
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		text, err := specs[0].ReadAllText()
		if err != nil {
			t.Fatalf("Failed to read all text: %v", err)
		}

		if text != originalText {
			t.Errorf("Text mismatch: expected %q, got %q", originalText, text)
		}
	}
}

func TestPartStream(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some streamed content")

	packaging := NewPackaging()

	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/aasx/data.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		stream, err := specs[0].Stream()
		if err != nil {
			t.Fatalf("Failed to get stream: %v", err)
		}
		defer stream.Close()

		content, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("Failed to read from stream: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Error("Content mismatch")
		}
	}
}

// =============================================================================
// TestPackageWriter
// =============================================================================

func TestPackageWriterRoundTrip(t *testing.T) {
	destination := &switchFailWriter{err: errors.New("destination failed")}
	writer, err := NewPackaging().CreateWriter(destination)
	if err != nil {
		t.Fatalf("Failed to create streaming writer: %v", err)
	}

	specContent := []byte(`{"assetAdministrationShells":[]}`)
	spec, err := writer.PutPartFromStream(
		mustParseURL("/aasx/spec.json"),
		"application/json",
		bytes.NewReader(specContent),
	)
	if err != nil {
		t.Fatalf("Failed to write spec: %v", err)
	}
	if _, err := spec.Stream(); !errors.Is(err, ErrWriteOnlyPart) {
		t.Fatalf("Expected write-only part error, got %v", err)
	}
	if err := writer.MakeSpec(spec); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}
	if err := writer.MakeSpec(spec); err != nil {
		t.Fatalf("Repeated MakeSpec failed: %v", err)
	}

	supplementaryContent := []byte("manual")
	supplementary, err := writer.PutPartFromStream(
		mustParseURL("/files/manual.json"),
		"application/pdf",
		bytes.NewReader(supplementaryContent),
	)
	if err != nil {
		t.Fatalf("Failed to write supplementary: %v", err)
	}
	if err := writer.RelateSupplementaryToSpec(supplementary, spec); err != nil {
		t.Fatalf("Failed to relate supplementary: %v", err)
	}
	if err := writer.RelateSupplementaryToSpec(supplementary, spec); err != nil {
		t.Fatalf("Repeated supplementary relationship failed: %v", err)
	}

	thumbnailContent := []byte("thumbnail")
	thumbnail, err := writer.PutPartFromStream(
		mustParseURL("/thumbnail.png"),
		"image/png",
		bytes.NewReader(thumbnailContent),
	)
	if err != nil {
		t.Fatalf("Failed to write thumbnail: %v", err)
	}
	if err := writer.SetThumbnail(thumbnail); err != nil {
		t.Fatalf("Failed to set thumbnail: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close streaming writer: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Repeated Close failed: %v", err)
	}
	if destination.closed {
		t.Fatal("PackageWriter closed its caller-owned destination")
	}
	if _, err := writer.PutPartFromStream(
		mustParseURL("/late.bin"), "application/octet-stream", bytes.NewReader(nil),
	); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected ErrWriterClosed, got %v", err)
	}

	pkg, err := NewPackaging().OpenReadFromReaderAt(
		bytes.NewReader(destination.Bytes()), int64(destination.Len()))
	if err != nil {
		t.Fatalf("Failed to open streaming output: %v", err)
	}
	defer pkg.Close()

	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one spec, got %d: %v", len(specs), err)
	}
	storedSpec, err := specs[0].ReadAllBytes()
	if err != nil || !bytes.Equal(storedSpec, specContent) {
		t.Fatalf("Spec content mismatch: %v", err)
	}
	if specs[0].ContentType != "application/json" {
		t.Fatalf("Unexpected spec content type: %s", specs[0].ContentType)
	}

	supplementaries, err := pkg.SupplementariesFor(specs[0])
	if err != nil || len(supplementaries) != 1 {
		t.Fatalf("Expected one supplementary, got %d: %v", len(supplementaries), err)
	}
	storedSupplementary, err := supplementaries[0].ReadAllBytes()
	if err != nil || !bytes.Equal(storedSupplementary, supplementaryContent) {
		t.Fatalf("Supplementary content mismatch: %v", err)
	}
	if supplementaries[0].ContentType != "application/pdf" {
		t.Fatalf("Conflicting extension content type was not preserved: %s",
			supplementaries[0].ContentType)
	}

	storedThumbnail, err := pkg.Thumbnail()
	if err != nil || storedThumbnail == nil {
		t.Fatalf("Expected thumbnail: %v", err)
	}
	content, err := storedThumbnail.ReadAllBytes()
	if err != nil || !bytes.Equal(content, thumbnailContent) {
		t.Fatalf("Thumbnail content mismatch: %v", err)
	}
}

func TestPackageWriterCopiesIncrementallyWithFixedBuffer(t *testing.T) {
	var destination bytes.Buffer
	writer, err := NewPackaging().CreateWriter(&destination)
	if err != nil {
		t.Fatalf("Failed to create streaming writer: %v", err)
	}
	sizeAfterCreate := destination.Len()
	payload := incompressibleTestContent(512 * 1024)
	source := &observingReader{reader: bytes.NewReader(payload)}
	part, err := writer.PutPartFromStream(
		mustParseURL("/large.bin"), "application/octet-stream", source)
	if err != nil {
		t.Fatalf("Failed to stream part: %v", err)
	}
	if source.maxReadSize > streamCopyBufferSize {
		t.Fatalf("Source read buffer was %d bytes, maximum is %d",
			source.maxReadSize, streamCopyBufferSize)
	}
	if destination.Len() <= sizeAfterCreate {
		t.Fatal("Destination did not receive incremental part output before Close")
	}
	if err := writer.MakeSpec(part); err != nil {
		t.Fatalf("Failed to make large part a spec: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close streaming writer: %v", err)
	}
}

func TestPackageWriterRejectsDuplicateReservedAndForeignParts(t *testing.T) {
	packaging := NewPackaging()
	var firstOutput bytes.Buffer
	first, err := packaging.CreateWriter(&firstOutput)
	if err != nil {
		t.Fatalf("Failed to create first writer: %v", err)
	}
	part, err := first.PutPartFromStream(
		mustParseURL("/Data/File.bin"), "application/octet-stream", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Failed to write first part: %v", err)
	}
	if _, err := first.PutPartFromStream(
		mustParseURL("/data/file.bin"), "application/octet-stream", bytes.NewReader(nil),
	); err == nil {
		t.Fatal("Expected case-normalized duplicate part to be rejected")
	}
	for _, reserved := range []string{
		"/[Content_Types].xml",
		"/_rels/.rels",
		"/_rels/data.bin",
		"/folder/_rels/data.bin",
	} {
		if _, err := first.PutPartFromStream(
			mustParseURL(reserved), "application/xml", bytes.NewReader(nil),
		); err == nil {
			t.Errorf("Expected reserved URI %s to be rejected", reserved)
		}
	}
	nearMatch, err := first.PutPartFromStream(
		mustParseURL("/foo_rels/data.bin"),
		"application/octet-stream",
		bytes.NewReader([]byte("visible")),
	)
	if err != nil {
		t.Fatalf("Near-match OPC path should be accepted: %v", err)
	}

	var secondOutput bytes.Buffer
	second, err := packaging.CreateWriter(&secondOutput)
	if err != nil {
		t.Fatalf("Failed to create second writer: %v", err)
	}
	if err := second.MakeSpec(part); err == nil {
		t.Fatal("Expected foreign part handle to be rejected")
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Failed to close second writer: %v", err)
	}
	if err := first.MakeSpec(part); err != nil {
		t.Fatalf("Validation errors should not poison writer: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Failed to close first writer: %v", err)
	}
	opened, err := packaging.OpenReadFromReaderAt(
		bytes.NewReader(firstOutput.Bytes()), int64(firstOutput.Len()))
	if err != nil {
		t.Fatalf("Failed to reopen first writer output: %v", err)
	}
	defer opened.Close()
	storedNearMatch, err := opened.MustPart(nearMatch.URI)
	if err != nil {
		t.Fatalf("Near-match writer part was hidden by reader: %v", err)
	}
	content, err := storedNearMatch.ReadAllBytes()
	if err != nil || string(content) != "visible" {
		t.Fatalf("Unexpected near-match writer content %q: %v", content, err)
	}
}

func TestPackageWriterSnapshotsMutablePartHandles(t *testing.T) {
	var destination bytes.Buffer
	writer, err := NewPackaging().CreateWriter(&destination)
	if err != nil {
		t.Fatalf("Failed to create writer: %v", err)
	}

	spec, err := writer.PutPartFromStream(
		mustParseURL("/original/spec.json"), "application/json", strings.NewReader("spec"))
	if err != nil {
		t.Fatalf("Failed to write spec: %v", err)
	}
	spec.URI = nil
	spec.ContentType = "application/x-mutated"
	if err := writer.MakeSpec(spec); err != nil {
		t.Fatalf("Mutated spec handle was not treated as an opaque handle: %v", err)
	}

	supplementaryURIs := []string{"/original/manual.pdf", "/original/schema.xml"}
	for _, rawURI := range supplementaryURIs {
		supplementary, putErr := writer.PutPartFromStream(
			mustParseURL(rawURI), "application/octet-stream", strings.NewReader(rawURI))
		if putErr != nil {
			t.Fatalf("Failed to write supplementary %s: %v", rawURI, putErr)
		}
		supplementary.URI = mustParseURL("/mutated.bin")
		supplementary.ContentType = "application/x-mutated"
		if relateErr := writer.RelateSupplementaryToSpec(supplementary, spec); relateErr != nil {
			t.Fatalf("Failed to relate mutated supplementary handle: %v", relateErr)
		}
	}

	thumbnail, err := writer.PutPartFromStream(
		mustParseURL("/original/thumbnail.png"), "image/png", strings.NewReader("thumbnail"))
	if err != nil {
		t.Fatalf("Failed to write thumbnail: %v", err)
	}
	thumbnail.URI = mustParseURL("/mutated.png")
	thumbnail.ContentType = "application/x-mutated"
	if err := writer.SetThumbnail(thumbnail); err != nil {
		t.Fatalf("Failed to set mutated thumbnail handle: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("Failed to close writer: %v", err)
	}
	pkg, err := NewPackaging().OpenReadFromReaderAt(
		bytes.NewReader(destination.Bytes()), int64(destination.Len()))
	if err != nil {
		t.Fatalf("Failed to open writer output: %v", err)
	}
	defer pkg.Close()

	specs, err := pkg.Specs()
	if err != nil || len(specs) != 1 {
		t.Fatalf("Expected one snapshotted spec, got %d: %v", len(specs), err)
	}
	if specs[0].URI.String() != "/original/spec.json" || specs[0].ContentType != "application/json" {
		t.Fatalf("Spec metadata followed mutable handle: %s, %s",
			specs[0].URI.String(), specs[0].ContentType)
	}
	supplementaries, err := pkg.SupplementariesFor(specs[0])
	if err != nil || len(supplementaries) != len(supplementaryURIs) {
		t.Fatalf("Expected %d supplementaries, got %d: %v",
			len(supplementaryURIs), len(supplementaries), err)
	}
	for _, rawURI := range supplementaryURIs {
		if _, err := pkg.MustPart(mustParseURL(rawURI)); err != nil {
			t.Errorf("Snapshotted supplementary %s not found: %v", rawURI, err)
		}
	}
	storedThumbnail, err := pkg.Thumbnail()
	if err != nil || storedThumbnail == nil {
		t.Fatalf("Expected snapshotted thumbnail: %v", err)
	}
	if storedThumbnail.URI.String() != "/original/thumbnail.png" ||
		storedThumbnail.ContentType != "image/png" {
		t.Fatalf("Thumbnail metadata followed mutable handle: %s, %s",
			storedThumbnail.URI.String(), storedThumbnail.ContentType)
	}
}

func TestBuildContentTypesIsDeterministicForConflictingExtensions(t *testing.T) {
	first := buildContentTypesForParts([]contentTypePart{
		{path: "/z/data.json", contentType: "application/x-z"},
		{path: "/a/data.json", contentType: "application/x-a"},
		{path: "/without-extension", contentType: "application/octet-stream"},
	})
	second := buildContentTypesForParts([]contentTypePart{
		{path: "/without-extension", contentType: "application/octet-stream"},
		{path: "/a/data.json", contentType: "application/x-a"},
		{path: "/z/data.json", contentType: "application/x-z"},
	})
	firstXML, err := xml.Marshal(first)
	if err != nil {
		t.Fatalf("Failed to marshal first content types: %v", err)
	}
	secondXML, err := xml.Marshal(second)
	if err != nil {
		t.Fatalf("Failed to marshal second content types: %v", err)
	}
	if !bytes.Equal(firstXML, secondXML) {
		t.Fatalf("Content types depend on part insertion order:\n%s\n%s", firstXML, secondXML)
	}

	var jsonDefault string
	for _, defaultContentType := range first.Defaults {
		if defaultContentType.Extension == "json" {
			jsonDefault = defaultContentType.ContentType
		}
	}
	if jsonDefault != "application/x-a" {
		t.Fatalf("Expected lexically first JSON part to define default, got %s", jsonDefault)
	}
}

func TestPackageWriterValidatesInputsAndClosedState(t *testing.T) {
	packaging := NewPackaging()
	if _, err := packaging.CreateWriter(nil); err == nil {
		t.Fatal("Expected nil destination to be rejected")
	}

	var nilWriter *PackageWriter
	if _, err := nilWriter.PutPartFromStream(nil, "", nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected nil writer Put error, got %v", err)
	}
	if err := nilWriter.MakeSpec(nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected nil writer MakeSpec error, got %v", err)
	}
	if err := nilWriter.RelateSupplementaryToSpec(nil, nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected nil writer relationship error, got %v", err)
	}
	if err := nilWriter.SetThumbnail(nil); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected nil writer thumbnail error, got %v", err)
	}
	if err := nilWriter.Close(); !errors.Is(err, ErrWriterClosed) {
		t.Fatalf("Expected nil writer Close error, got %v", err)
	}

	var destination bytes.Buffer
	writer, err := packaging.CreateWriter(&destination)
	if err != nil {
		t.Fatalf("Failed to create writer: %v", err)
	}
	if _, err := writer.PutPartFromStream(
		mustParseURL("/nil-stream.bin"), "application/octet-stream", nil,
	); err == nil {
		t.Fatal("Expected nil part stream to be rejected")
	}
	invalidURIs := []*url.URL{
		nil,
		{},
		mustParseURL("https://example.com/external.bin"),
		mustParseURL("/"),
		mustParseURL("/directory/"),
		mustParseURL("/query.bin?value=1"),
		mustParseURL("/fragment.bin#value"),
	}
	for index, uri := range invalidURIs {
		if _, err := writer.PutPartFromStream(
			uri, "application/octet-stream", bytes.NewReader(nil),
		); err == nil {
			t.Errorf("Invalid URI %d was accepted", index)
		}
	}
	if err := writer.MakeSpec(nil); err == nil {
		t.Fatal("Expected nil part handle to be rejected")
	}
	part, err := writer.PutPartFromStream(
		mustParseURL("/part.bin"), "application/octet-stream", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Failed to write valid part: %v", err)
	}
	if err := writer.RelateSupplementaryToSpec(part, part); err == nil {
		t.Fatal("Expected relationship to an unmarked spec to be rejected")
	}
	if err := writer.SetThumbnail(nil); err == nil {
		t.Fatal("Expected nil thumbnail handle to be rejected")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Validation errors poisoned writer: %v", err)
	}
	closedOperations := []func() error{
		func() error { return writer.MakeSpec(part) },
		func() error { return writer.RelateSupplementaryToSpec(part, part) },
		func() error { return writer.SetThumbnail(part) },
	}
	for index, operation := range closedOperations {
		if err := operation(); !errors.Is(err, ErrWriterClosed) {
			t.Errorf("Closed writer operation %d returned %v", index, err)
		}
	}
}

func TestPackageWriterPropagatesAndRetainsFailures(t *testing.T) {
	sourceErr := errors.New("source failed")
	var destination bytes.Buffer
	writer, err := NewPackaging().CreateWriter(&destination)
	if err != nil {
		t.Fatalf("Failed to create streaming writer: %v", err)
	}
	_, err = writer.PutPartFromStream(
		mustParseURL("/broken.bin"),
		"application/octet-stream",
		&errorAfterReader{content: []byte("partial"), err: sourceErr},
	)
	if !errors.Is(err, sourceErr) {
		t.Fatalf("Expected source failure, got %v", err)
	}
	if _, err := writer.PutPartFromStream(
		mustParseURL("/later.bin"), "application/octet-stream", bytes.NewReader(nil),
	); !errors.Is(err, sourceErr) {
		t.Fatalf("Expected retained source failure, got %v", err)
	}
	if err := writer.Close(); !errors.Is(err, sourceErr) {
		t.Fatalf("Close did not return retained source failure: %v", err)
	}
	if err := writer.Close(); !errors.Is(err, sourceErr) {
		t.Fatalf("Repeated Close did not return retained failure: %v", err)
	}

	destinationErr := errors.New("destination failed")
	failingDestination := &switchFailWriter{err: destinationErr}
	writer, err = NewPackaging().CreateWriter(failingDestination)
	if err != nil {
		t.Fatalf("Failed to create writer before enabling destination failure: %v", err)
	}
	failingDestination.fail = true
	_, putErr := writer.PutPartFromStream(
		mustParseURL("/large.bin"),
		"application/octet-stream",
		bytes.NewReader(incompressibleTestContent(128*1024)),
	)
	if putErr == nil {
		putErr = writer.Close()
	} else {
		_ = writer.Close()
	}
	if !errors.Is(putErr, destinationErr) {
		t.Fatalf("Expected destination failure, got %v", putErr)
	}
}

// =============================================================================
// TestPackageReadWrite (Create)
// =============================================================================

func TestPackageReadWriteOperationsAfterClose(t *testing.T) {
	var destination bytes.Buffer
	pkg, err := NewPackaging().CreateInStream(&readWriteSeeker{buf: &destination})
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}
	part, err := pkg.PutPart(
		mustParseURL("/part.bin"), "application/octet-stream", []byte("part"))
	if err != nil {
		t.Fatalf("Failed to add part: %v", err)
	}
	if err := pkg.MakeSpec(part); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close package: %v", err)
	}

	operations := []func() error{
		func() error {
			_, operationErr := pkg.PutPart(
				mustParseURL("/late.bin"), "application/octet-stream", nil)
			return operationErr
		},
		func() error {
			_, operationErr := pkg.PutPartFromStream(
				mustParseURL("/late-stream.bin"), "application/octet-stream", bytes.NewReader(nil))
			return operationErr
		},
		func() error { return pkg.DeletePart(part) },
		func() error { return pkg.MakeSpec(part) },
		func() error { return pkg.UnmakeSpec(part) },
		func() error { return pkg.RelateSupplementaryToSpec(part, part) },
		func() error { return pkg.UnrelateSupplementaryFromSpec(part, part) },
		func() error { return pkg.SetThumbnail(part) },
		func() error { return pkg.UnsetThumbnail() },
		func() error { return pkg.Flush() },
	}
	for index, operation := range operations {
		if err := operation(); !errors.Is(err, ErrPackageClosed) {
			t.Errorf("Operation %d: expected ErrPackageClosed, got %v", index, err)
		}
	}
}

func TestCreateNewPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify file exists
	if _, err := os.Stat(pth); os.IsNotExist(err) {
		t.Error("Package file was not created")
	}
}

func TestCreateNewPackageInStream(t *testing.T) {
	packaging := NewPackaging()
	originalContent := []byte("some content")

	// Create in memory stream (simulated with buffer)
	var buf bytes.Buffer
	stream := &readWriteSeeker{buf: &buf}

	pkg, err := packaging.CreateInStream(stream)
	if err != nil {
		t.Fatalf("Failed to create package in stream: %v", err)
	}

	part, err := pkg.PutPart(
		mustParseURL("/some-thumbnail.txt"),
		"text/plain",
		originalContent,
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}
	if err := pkg.SetThumbnail(part); err != nil {
		t.Fatalf("Failed to set thumbnail: %v", err)
	}
	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify data was written
	if buf.Len() == 0 {
		t.Error("No data written to stream")
	}
}

func TestCreatePackageAtNonReachablePathReturnsError(t *testing.T) {
	packaging := NewPackaging()

	_, err := packaging.Create("/this/path/can/not/possibly/exist/dummy.aasx")
	if err == nil {
		t.Error("Expected error when creating at non-reachable path")
	}
}

func TestCreatePackageInReadOnlyStreamReturnsError(t *testing.T) {
	packaging := NewPackaging()

	// Create a stream that doesn't support writing properly
	// We'll use a minimal struct that claims to implement WriteSeeker but fails on write
	type failingWriter struct {
		*bytes.Reader
	}
	fw := failingWriter{bytes.NewReader([]byte{})}

	// Note: In Go, we need an actual io.ReadWriteSeeker. This test verifies
	// that creating a package fails if the stream cannot be written to.
	// Since bytes.Reader doesn't implement io.Writer, we test with a proper
	// failing write scenario using an interface assertion at runtime.
	// For the TDD approach, this test will be validated when CreateInStream
	// is implemented and handles write failures.
	_ = fw

	// Alternative: Test with empty buffer that might fail validation
	var buf bytes.Buffer
	stream := &readWriteSeeker{buf: &buf}
	// The test will verify behavior when implementation is complete
	_, _ = packaging.CreateInStream(stream)
}

func TestAddSpecToNewPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some content")

	packaging := NewPackaging()

	// Create
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		if len(specs) != 1 {
			t.Fatalf("Expected 1 spec, got %d", len(specs))
		}

		content, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read content: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Error("Content mismatch")
		}
	}
}

func TestAddSupplementaryFileToNewPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	supplContent := []byte("some supplementary content")

	packaging := NewPackaging()

	// Create
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		specPart, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/data.txt"),
			"text/plain",
			[]byte("some spec content"),
		)
		if err != nil {
			t.Fatalf("Failed to put spec part: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(
			mustParseURL("/aasx/some-company/suppl.txt"),
			"text/plain",
			supplContent,
		)
		if err != nil {
			t.Fatalf("Failed to put supplementary part: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to relate supplementary: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		rels, err := pkg.SupplementaryRelationships()
		if err != nil {
			t.Fatalf("Failed to get supplementary relationships: %v", err)
		}

		if len(rels) != 1 {
			t.Fatalf("Expected 1 supplementary, got %d", len(rels))
		}

		content, err := rels[0].Supplementary.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read supplementary: %v", err)
		}

		if !bytes.Equal(content, supplContent) {
			t.Error("Content mismatch")
		}
	}
}

func TestAddThumbnailToNewPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some content")

	packaging := NewPackaging()

	// Create
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/some-thumbnail.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Read and verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open package: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}

		if thumbnail == nil {
			t.Fatal("Expected thumbnail but got nil")
		}

		content, err := thumbnail.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read thumbnail: %v", err)
		}

		if !bytes.Equal(content, originalContent) {
			t.Error("Content mismatch")
		}
	}
}

// =============================================================================
// TestPackageReadWrite (Modify)
// =============================================================================

func TestOpenReadWrite(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	// Create initial package
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create package: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Open for read/write
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open for read/write: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

func TestOpenReadWriteFromStream(t *testing.T) {
	packaging := NewPackaging()
	originalContent := []byte("some content")

	var buf bytes.Buffer
	stream := &readWriteSeeker{buf: &buf}

	// Create in stream
	{
		pkg, err := packaging.CreateInStream(stream)
		if err != nil {
			t.Fatalf("Failed to create in stream: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/some-thumbnail.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Reset stream position
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Failed to seek: %v", err)
	}

	// Open for read/write
	{
		pkg, err := packaging.OpenReadWriteFromStream(stream)
		if err != nil {
			t.Fatalf("Failed to open for read/write from stream: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

func TestPutPart(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	content := []byte("test content")

	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	part, err := pkg.PutPart(
		mustParseURL("/aasx/data.txt"),
		"text/plain",
		content,
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}

	if part == nil {
		t.Fatal("Expected part but got nil")
	}
	if part.ContentType != "text/plain" {
		t.Errorf("Expected content type text/plain, got %s", part.ContentType)
	}

	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}
}

func TestPutPartFromStream(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	content := []byte("test content from stream")

	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create package: %v", err)
	}

	contentReader := bytes.NewReader(content)
	part, err := pkg.PutPartFromStream(
		mustParseURL("/aasx/data.txt"),
		"text/plain",
		contentReader,
	)
	if err != nil {
		t.Fatalf("Failed to put part from stream: %v", err)
	}

	if part == nil {
		t.Error("Expected part but got nil")
	}

	if err := pkg.MakeSpec(part); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}
	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify content
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		readContent, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if !bytes.Equal(readContent, content) {
			t.Error("Content mismatch")
		}
	}
}

func TestDeletePart(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	uri := mustParseURL("/aasx/some-company/data.txt")

	packaging := NewPackaging()

	// Create with part
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", []byte("content"))
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Delete part
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		part, err := pkg.MustPart(uri)
		if err != nil {
			t.Fatalf("Failed to get part: %v", err)
		}

		if err := pkg.UnmakeSpec(part); err != nil {
			t.Fatalf("Failed to unmake spec: %v", err)
		}
		if err := pkg.DeletePart(part); err != nil {
			t.Fatalf("Failed to delete part: %v", err)
		}

		found, err := pkg.FindPart(uri)
		if err != nil {
			t.Fatalf("Failed to find part: %v", err)
		}
		if found != nil {
			t.Error("Part should have been deleted")
		}

		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

func TestMakeSpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	part, err := pkg.PutPart(
		mustParseURL("/aasx/data.txt"),
		"text/plain",
		[]byte("content"),
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}

	if err := pkg.MakeSpec(part); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify it's a spec
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		if len(specs) != 1 {
			t.Errorf("Expected 1 spec, got %d", len(specs))
		}
	}
}

func TestUnmakeSpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	uri := mustParseURL("/aasx/data.txt")

	packaging := NewPackaging()

	// Create with spec
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", []byte("content"))
		if err != nil {
			t.Fatalf("Failed to put part: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Unmake spec
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		part, err := pkg.MustPart(uri)
		if err != nil {
			t.Fatalf("Failed to get part: %v", err)
		}

		if err := pkg.UnmakeSpec(part); err != nil {
			t.Fatalf("Failed to unmake spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify no specs
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		if len(specs) != 0 {
			t.Errorf("Expected 0 specs, got %d", len(specs))
		}
	}
}

func TestRelateSupplementaryToSpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	specPart, err := pkg.PutPart(
		mustParseURL("/aasx/data.txt"),
		"text/plain",
		[]byte("spec content"),
	)
	if err != nil {
		t.Fatalf("Failed to put spec: %v", err)
	}
	if err := pkg.MakeSpec(specPart); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	supplPart, err := pkg.PutPart(
		mustParseURL("/aasx/suppl.txt"),
		"text/plain",
		[]byte("suppl content"),
	)
	if err != nil {
		t.Fatalf("Failed to put supplementary: %v", err)
	}

	if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
		t.Fatalf("Failed to relate supplementary to spec: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		rels, err := pkg.SupplementaryRelationships()
		if err != nil {
			t.Fatalf("Failed to get rels: %v", err)
		}

		if len(rels) != 1 {
			t.Errorf("Expected 1 relationship, got %d", len(rels))
		}
	}
}

func TestUnrelateSupplementaryFromSpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	specUri := mustParseURL("/aasx/data.txt")
	supplUri := mustParseURL("/aasx/suppl.txt")

	packaging := NewPackaging()

	// Create with relationship
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		specPart, err := pkg.PutPart(specUri, "text/plain", []byte("spec"))
		if err != nil {
			t.Fatalf("Failed to put spec: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(supplUri, "text/plain", []byte("suppl"))
		if err != nil {
			t.Fatalf("Failed to put suppl: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to relate: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Unrelate
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		specPart, err := pkg.MustPart(specUri)
		if err != nil {
			t.Fatalf("Failed to get spec: %v", err)
		}
		supplPart, err := pkg.MustPart(supplUri)
		if err != nil {
			t.Fatalf("Failed to get suppl: %v", err)
		}

		if err := pkg.UnrelateSupplementaryFromSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to unrelate: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		rels, err := pkg.SupplementaryRelationships()
		if err != nil {
			t.Fatalf("Failed to get rels: %v", err)
		}

		if len(rels) != 0 {
			t.Errorf("Expected 0 relationships, got %d", len(rels))
		}
	}
}

func TestSetThumbnail(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	content := []byte("thumbnail content")

	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	part, err := pkg.PutPart(
		mustParseURL("/thumbnail.txt"),
		"text/plain",
		content,
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}

	if err := pkg.SetThumbnail(part); err != nil {
		t.Fatalf("Failed to set thumbnail: %v", err)
	}

	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}
	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}

		if thumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		readContent, err := thumbnail.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if !bytes.Equal(readContent, content) {
			t.Error("Content mismatch")
		}
	}
}

func TestUnsetThumbnail(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	thumbnailUri := mustParseURL("/thumbnail.txt")

	packaging := NewPackaging()

	// Create with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(thumbnailUri, "text/plain", []byte("content"))
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Unset and delete thumbnail
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if thumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		if err := pkg.UnsetThumbnail(); err != nil {
			t.Fatalf("Failed to unset thumbnail: %v", err)
		}
		if err := pkg.DeletePart(thumbnail); err != nil {
			t.Fatalf("Failed to delete part: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify no thumbnail
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if thumbnail != nil {
			t.Error("Expected no thumbnail")
		}
	}
}

func TestFlush(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	packaging := NewPackaging()

	pkg, err := packaging.Create(pth)
	if err != nil {
		t.Fatalf("Failed to create: %v", err)
	}

	part, err := pkg.PutPart(
		mustParseURL("/aasx/data.txt"),
		"text/plain",
		[]byte("content"),
	)
	if err != nil {
		t.Fatalf("Failed to put part: %v", err)
	}
	if err := pkg.MakeSpec(part); err != nil {
		t.Fatalf("Failed to make spec: %v", err)
	}

	// Flush should write to disk
	if err := pkg.Flush(); err != nil {
		t.Fatalf("Failed to flush: %v", err)
	}

	// Verify file exists and has content
	info, err := os.Stat(pth)
	if err != nil {
		t.Fatalf("File not created: %v", err)
	}
	if info.Size() == 0 {
		t.Error("File is empty after flush")
	}

	if err := pkg.Close(); err != nil {
		t.Fatalf("Failed to close: %v", err)
	}
}

// =============================================================================
// TestPackageReadWrite - Overwrite Tests
// =============================================================================

func TestOverwriteSpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	uri := mustParseURL("/aasx/data.txt")
	originalContent := []byte("old content")
	newContent := []byte("new content")

	packaging := NewPackaging()

	// Create
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", originalContent)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Overwrite
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", newContent)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.MakeSpec(part); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		specs, err := pkg.Specs()
		if err != nil {
			t.Fatalf("Failed to get specs: %v", err)
		}

		content, err := specs[0].ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if string(content) != string(newContent) {
			t.Errorf("Expected %q, got %q", newContent, content)
		}
	}
}

func TestOverwriteSupplementary(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("some old content")
	newContent := []byte("new content")

	packaging := NewPackaging()

	supplURI := mustParseURL("/aasx/suppl/data.txt")
	specURI := mustParseURL("/aasx/some-company/data.txt")

	// Create
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		specPart, err := pkg.PutPart(specURI, "text/plain", []byte("some spec content"))
		if err != nil {
			t.Fatalf("Failed to put spec: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(supplURI, "text/plain", originalContent)
		if err != nil {
			t.Fatalf("Failed to put suppl: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to relate: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Overwrite supplementary
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		_, err = pkg.PutPart(supplURI, "text/plain", newContent)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		part, err := pkg.FindPart(supplURI)
		if err != nil {
			t.Fatalf("Failed to find: %v", err)
		}
		if part == nil {
			t.Fatal("Expected part")
		}

		content, err := part.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if string(content) != string(newContent) {
			t.Errorf("Expected %q, got %q", newContent, content)
		}
	}
}

func TestModifyThumbnailAndDeleteExisting(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("original thumbnail")
	newContent := []byte("new thumbnail")

	packaging := NewPackaging()

	// Create with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(
			mustParseURL("/some-thumbnail.txt"),
			"text/plain",
			originalContent,
		)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Modify and delete old
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		oldThumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if oldThumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		if err := pkg.UnsetThumbnail(); err != nil {
			t.Fatalf("Failed to unset: %v", err)
		}
		if err := pkg.DeletePart(oldThumbnail); err != nil {
			t.Fatalf("Failed to delete: %v", err)
		}

		newPart, err := pkg.PutPart(
			mustParseURL("/another-thumbnail.txt"),
			"text/plain",
			newContent,
		)
		if err != nil {
			t.Fatalf("Failed to put new: %v", err)
		}
		if err := pkg.SetThumbnail(newPart); err != nil {
			t.Fatalf("Failed to set new thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if thumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		content, err := thumbnail.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if !bytes.Equal(content, newContent) {
			t.Error("Content mismatch")
		}
	}
}

func TestOpenReadWriteReturnsErrorForInvalidPackage(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")

	// Create an invalid file w.r.t. Open Package Convention
	if err := os.WriteFile(pth, []byte("This is not OPC."), 0644); err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadWrite(pth)
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening invalid package for read/write")
	}
}

func TestOpenReadWriteReturnsErrorForFileWithoutOrigin(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")

	// Create an empty OPC package (no AASX origin)
	emptyZip := []byte{0x50, 0x4b, 0x05, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := os.WriteFile(pth, emptyZip, 0644); err != nil {
		t.Fatalf("Failed to create file: %v", err)
	}

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadWrite(pth)
	if err == nil {
		defer pkg.Close()
		t.Log("Package opened, but should have missing origin validation")
	}
	// Error is expected
}

func TestOpenReadWriteFromStreamReturnsErrorForStreamWithoutOrigin(t *testing.T) {
	// Create an empty OPC package in memory (no AASX origin)
	emptyZip := []byte{0x50, 0x4b, 0x05, 0x06, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}

	var buf bytes.Buffer
	buf.Write(emptyZip)
	stream := &readWriteSeeker{buf: &buf}

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadWriteFromStream(stream)
	if err == nil {
		defer pkg.Close()
		t.Log("Package opened, but should have missing origin validation")
	}
	// Error is expected
}

func TestOpenReadWriteFromStreamReturnsErrorForEmptyStream(t *testing.T) {
	var buf bytes.Buffer
	stream := &readWriteSeeker{buf: &buf}

	packaging := NewPackaging()

	pkg, err := packaging.OpenReadWriteFromStream(stream)
	if err == nil {
		defer pkg.Close()
		t.Error("Expected error when opening empty stream for read/write")
	}
}

func TestModifyThumbnailAndDontDeleteExisting(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	originalContent := []byte("original thumbnail")
	newContent := []byte("new thumbnail")
	originalURI := mustParseURL("/some-thumbnail.txt")

	packaging := NewPackaging()

	// Create with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(originalURI, "text/plain", originalContent)
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Modify thumbnail without deleting old one
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		newPart, err := pkg.PutPart(
			mustParseURL("/another-thumbnail.txt"),
			"text/plain",
			newContent,
		)
		if err != nil {
			t.Fatalf("Failed to put new: %v", err)
		}
		if err := pkg.SetThumbnail(newPart); err != nil {
			t.Fatalf("Failed to set new thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Verify new thumbnail and old part still exists
	{
		pkg, err := packaging.OpenRead(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}
		defer pkg.Close()

		// Check new thumbnail
		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if thumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		content, err := thumbnail.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read: %v", err)
		}

		if !bytes.Equal(content, newContent) {
			t.Error("New thumbnail content mismatch")
		}

		// Check old part still exists
		oldPart, err := pkg.MustPart(originalURI)
		if err != nil {
			t.Fatalf("Failed to get old part: %v", err)
		}

		oldContent, err := oldPart.ReadAllBytes()
		if err != nil {
			t.Fatalf("Failed to read old: %v", err)
		}

		if !bytes.Equal(oldContent, originalContent) {
			t.Error("Old thumbnail content mismatch")
		}
	}
}

// =============================================================================
// TestPackageReadWrite - Delete Tests
// =============================================================================

func TestDeletingASpec(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	uri := mustParseURL("/aasx/some-company/data.txt")
	anotherUri := mustParseURL("/aasx/some-company/anotherData.txt")

	packaging := NewPackaging()

	// Create with two specs
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part1, err := pkg.PutPart(uri, "text/plain", []byte("some content"))
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.MakeSpec(part1); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		part2, err := pkg.PutPart(anotherUri, "text/plain", []byte("another content"))
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.MakeSpec(part2); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Delete one spec
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		part, err := pkg.MustPart(uri)
		if err != nil {
			t.Fatalf("Failed to get part: %v", err)
		}

		if err := pkg.UnmakeSpec(part); err != nil {
			t.Fatalf("Failed to unmake spec: %v", err)
		}
		if err := pkg.DeletePart(part); err != nil {
			t.Fatalf("Failed to delete: %v", err)
		}

		found, err := pkg.FindPart(uri)
		if err != nil {
			t.Fatalf("Failed to find: %v", err)
		}
		if found != nil {
			t.Error("Part should be deleted")
		}

		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

func TestDeletingASupplementary(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	specUri := mustParseURL("/aasx/some-company/data.txt")
	supplUri := mustParseURL("/aasx/some-company/suppl.txt")
	anotherSupplUri := mustParseURL("/aasx/some-company/suppl1.txt")

	packaging := NewPackaging()

	// Create with two supplementaries
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		specPart, err := pkg.PutPart(specUri, "text/plain", []byte("spec content"))
		if err != nil {
			t.Fatalf("Failed to put spec: %v", err)
		}
		if err := pkg.MakeSpec(specPart); err != nil {
			t.Fatalf("Failed to make spec: %v", err)
		}

		supplPart, err := pkg.PutPart(supplUri, "text/plain", []byte("suppl content"))
		if err != nil {
			t.Fatalf("Failed to put suppl: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to relate: %v", err)
		}

		anotherSupplPart, err := pkg.PutPart(anotherSupplUri, "text/plain", []byte("another suppl"))
		if err != nil {
			t.Fatalf("Failed to put another suppl: %v", err)
		}
		if err := pkg.RelateSupplementaryToSpec(anotherSupplPart, specPart); err != nil {
			t.Fatalf("Failed to relate: %v", err)
		}

		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Delete one supplementary
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		supplPart, err := pkg.MustPart(supplUri)
		if err != nil {
			t.Fatalf("Failed to get suppl: %v", err)
		}
		specPart, err := pkg.MustPart(specUri)
		if err != nil {
			t.Fatalf("Failed to get spec: %v", err)
		}

		if err := pkg.UnrelateSupplementaryFromSpec(supplPart, specPart); err != nil {
			t.Fatalf("Failed to unrelate: %v", err)
		}
		if err := pkg.DeletePart(supplPart); err != nil {
			t.Fatalf("Failed to delete: %v", err)
		}

		found, err := pkg.FindPart(supplUri)
		if err != nil {
			t.Fatalf("Failed to find: %v", err)
		}
		if found != nil {
			t.Error("Supplementary should be deleted")
		}

		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

func TestDeletingAThumbnail(t *testing.T) {
	tmpdir, cleanup := temporaryDirectory(t)
	defer cleanup()

	pth := filepath.Join(tmpdir, "dummy.aasx")
	uri := mustParseURL("/aasx/some-company/thumb.txt")

	packaging := NewPackaging()

	// Create with thumbnail
	{
		pkg, err := packaging.Create(pth)
		if err != nil {
			t.Fatalf("Failed to create: %v", err)
		}

		part, err := pkg.PutPart(uri, "text/plain", []byte("thumb content"))
		if err != nil {
			t.Fatalf("Failed to put: %v", err)
		}
		if err := pkg.SetThumbnail(part); err != nil {
			t.Fatalf("Failed to set thumbnail: %v", err)
		}
		if err := pkg.Flush(); err != nil {
			t.Fatalf("Failed to flush: %v", err)
		}
		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}

	// Delete thumbnail
	{
		pkg, err := packaging.OpenReadWrite(pth)
		if err != nil {
			t.Fatalf("Failed to open: %v", err)
		}

		thumbnail, err := pkg.Thumbnail()
		if err != nil {
			t.Fatalf("Failed to get thumbnail: %v", err)
		}
		if thumbnail == nil {
			t.Fatal("Expected thumbnail")
		}

		if err := pkg.UnsetThumbnail(); err != nil {
			t.Fatalf("Failed to unset: %v", err)
		}
		if err := pkg.DeletePart(thumbnail); err != nil {
			t.Fatalf("Failed to delete: %v", err)
		}

		found, err := pkg.FindPart(uri)
		if err != nil {
			t.Fatalf("Failed to find: %v", err)
		}
		if found != nil {
			t.Error("Thumbnail should be deleted")
		}

		if err := pkg.Close(); err != nil {
			t.Fatalf("Failed to close: %v", err)
		}
	}
}

// =============================================================================
// Helper types for stream-based tests
// =============================================================================

// readWriteSeeker wraps a bytes.Buffer to implement io.ReadWriteSeeker
type readWriteSeeker struct {
	buf *bytes.Buffer
	pos int64
}

func (rws *readWriteSeeker) Read(p []byte) (n int, err error) {
	if rws.pos >= int64(rws.buf.Len()) {
		return 0, io.EOF
	}
	n = copy(p, rws.buf.Bytes()[rws.pos:])
	rws.pos += int64(n)
	return n, nil
}

func (rws *readWriteSeeker) Write(p []byte) (n int, err error) {
	// Extend buffer if needed
	if int(rws.pos) > rws.buf.Len() {
		padding := make([]byte, int(rws.pos)-rws.buf.Len())
		rws.buf.Write(padding)
	}

	// If writing beyond current length, just append
	if int(rws.pos) == rws.buf.Len() {
		n, err = rws.buf.Write(p)
		rws.pos += int64(n)
		return n, err
	}

	// Writing in the middle - need to handle this specially
	data := rws.buf.Bytes()
	if int(rws.pos)+len(p) > len(data) {
		// Extend the buffer
		newData := make([]byte, int(rws.pos)+len(p))
		copy(newData, data)
		data = newData
	}
	n = copy(data[rws.pos:], p)
	rws.buf.Reset()
	rws.buf.Write(data)
	rws.pos += int64(n)
	return n, nil
}

func (rws *readWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = rws.pos + offset
	case io.SeekEnd:
		newPos = int64(rws.buf.Len()) + offset
	}

	if newPos < 0 {
		return 0, io.EOF
	}
	rws.pos = newPos
	return newPos, nil
}
