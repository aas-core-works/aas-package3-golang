// Package aasx provides functionality for reading and writing packaged file format
// of an Asset Administration Shell (AAS) v3.
//
// The package follows the Open Packaging Convention (OPC) standard for AASX files.
package aasx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Relation types used in AASX packages.
const (
	PreferredAasxRelationshipsPrefix  = "http://admin-shell.io/aasx/relationships/"
	DeprecatedAasxRelationshipsPrefix = "http://www.admin-shell.io/aasx/relationships/"

	RelationTypeAasxOrigin        = PreferredAasxRelationshipsPrefix + "aasx-origin"
	RelationTypeAasxSpec          = PreferredAasxRelationshipsPrefix + "aas-spec"
	RelationTypeAasxSupplementary = PreferredAasxRelationshipsPrefix + "aas-suppl"
	RelationTypeThumbnail         = "http://schemas.openxmlformats.org/package/2006" +
		"/relationships/metadata/thumbnail"
)

// OPC XML namespaces
const (
	opcRelationshipNamespace = "http://schemas.openxmlformats.org/package/2006/relationships"
	opcContentTypesNamespace = "http://schemas.openxmlformats.org/package/2006/content-types"
)

// Common errors
var (
	ErrNoOriginPart        = errors.New("no origin part found")
	ErrPartNotFound        = errors.New("part not found")
	ErrInvalidFormat       = errors.New("invalid package format")
	ErrReaderLimitExceeded = errors.New("reader limit exceeded")
	ErrPackageClosed       = errors.New("package is closed")
	ErrWriterClosed        = errors.New("package writer is closed")
	ErrWriteOnlyPart       = errors.New("part belongs to a write-only package")
)

// region OPC XML Types

// relationshipsXML represents a .rels file in an OPC package
type relationshipsXML struct {
	XMLName       xml.Name          `xml:"Relationships"`
	Xmlns         string            `xml:"xmlns,attr"`
	Relationships []relationshipXML `xml:"Relationship"`
}

// relationshipXML represents a single relationship entry
type relationshipXML struct {
	ID         string `xml:"Id,attr"`
	Type       string `xml:"Type,attr"`
	Target     string `xml:"Target,attr"`
	TargetMode string `xml:"TargetMode,attr,omitempty"`
}

// contentTypesXML represents the [Content_Types].xml file
type contentTypesXML struct {
	XMLName   xml.Name       `xml:"Types"`
	Xmlns     string         `xml:"xmlns,attr"`
	Defaults  []defaultType  `xml:"Default"`
	Overrides []overrideType `xml:"Override"`
}

// defaultType represents a default content type mapping
type defaultType struct {
	Extension   string `xml:"Extension,attr"`
	ContentType string `xml:"ContentType,attr"`
}

// overrideType represents an override content type mapping
type overrideType struct {
	PartName    string `xml:"PartName,attr"`
	ContentType string `xml:"ContentType,attr"`
}

// relationship represents an OPC relationship (internal representation)
type relationship struct {
	id         string
	relType    string
	target     string
	targetMode string
}

// endregion

// region Lazy Reader Options and Helpers

const streamCopyBufferSize = 32 * 1024

// ReaderOption configures the resource limits applied while opening and
// reading a package. A limit of zero means unlimited.
type ReaderOption func(*readerOptions)

type readerOptions struct {
	maxPartCount          uint64
	maxOPCMetadataBytes   uint64
	maxPartExpandedBytes  uint64
	maxTotalExpandedBytes uint64
}

// WithMaxPartCount limits the number of non-directory ZIP entries.
func WithMaxPartCount(max uint64) ReaderOption {
	return func(options *readerOptions) {
		options.maxPartCount = max
	}
}

// WithMaxOPCMetadataBytes limits the combined expanded size of
// [Content_Types].xml and relationship parts.
func WithMaxOPCMetadataBytes(max uint64) ReaderOption {
	return func(options *readerOptions) {
		options.maxOPCMetadataBytes = max
	}
}

// WithMaxPartExpandedBytes limits the expanded size of each payload part.
func WithMaxPartExpandedBytes(max uint64) ReaderOption {
	return func(options *readerOptions) {
		options.maxPartExpandedBytes = max
	}
}

// WithMaxTotalExpandedBytes limits the combined expanded size of all payload
// parts. OPC metadata is governed by WithMaxOPCMetadataBytes instead.
func WithMaxTotalExpandedBytes(max uint64) ReaderOption {
	return func(options *readerOptions) {
		options.maxTotalExpandedBytes = max
	}
}

func applyReaderOptions(options []ReaderOption) readerOptions {
	var result readerOptions
	for _, option := range options {
		if option != nil {
			option(&result)
		}
	}
	return result
}

// readSeekerAt adapts an io.ReadSeeker to io.ReaderAt. ZIP readers can issue
// concurrent ReadAt calls, so seeking and reading must be one critical section.
type readSeekerAt struct {
	stream io.ReadSeeker
	mu     sync.Mutex
}

func (reader *readSeekerAt) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, fmt.Errorf("negative read offset: %d", offset)
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()

	if _, err := reader.stream.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}

	read, err := io.ReadFull(reader.stream, buffer)
	if err == io.ErrUnexpectedEOF {
		err = io.EOF
	}
	return read, err
}

type boundedReadCloser struct {
	stream    io.ReadCloser
	remaining uint64
	label     string
}

func (reader *boundedReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}

	if reader.remaining == 0 {
		var probe [1]byte
		read, err := reader.stream.Read(probe[:])
		if read > 0 {
			return 0, fmt.Errorf("%w: %s", ErrReaderLimitExceeded, reader.label)
		}
		return 0, err
	}

	if uint64(len(buffer)) > reader.remaining {
		buffer = buffer[:int(reader.remaining)]
	}
	read, err := reader.stream.Read(buffer)
	reader.remaining -= uint64(read)
	return read, err
}

func (reader *boundedReadCloser) Close() error {
	return reader.stream.Close()
}

func readZipFile(file zipFile, max uint64, limited bool, label string) ([]byte, error) {
	if limited && file.uncompressedSize() > max {
		return nil, fmt.Errorf(
			"%w: %s is %d bytes, maximum is %d",
			ErrReaderLimitExceeded,
			label,
			file.uncompressedSize(),
			max,
		)
	}

	stream, err := file.open()
	if err != nil {
		return nil, err
	}
	if limited {
		stream = &boundedReadCloser{
			stream:    stream,
			remaining: max,
			label:     label,
		}
	}

	var result bytes.Buffer
	buffer := make([]byte, streamCopyBufferSize)
	_, readErr := io.CopyBuffer(&result, struct{ io.Reader }{stream}, buffer)
	closeErr := stream.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return result.Bytes(), nil
}

// zipFile is the small portion of archive/zip.File used by the bounded reader
// helper. Keeping it as an interface makes limit and close failures testable.
type zipFile interface {
	open() (io.ReadCloser, error)
	uncompressedSize() uint64
}

type archiveZipFile struct {
	file *zip.File
}

func (file archiveZipFile) open() (io.ReadCloser, error) {
	return file.file.Open()
}

func (file archiveZipFile) uncompressedSize() uint64 {
	return file.file.UncompressedSize64
}

// endregion

// region Part

// Part represents a part of an AAS package.
type Part struct {
	// URI is the location of the part within the package.
	URI *url.URL

	// ContentType is the MIME type of the part.
	ContentType string

	// content holds the byte content of the part
	content []byte

	// archiveFile references the underlying ZIP entry for lazy read packages.
	archiveFile *zip.File

	// pkg is a reference back to the package.
	pkg *packageBase

	// writeOnly identifies lightweight handles returned by PackageWriter.
	writeOnly bool
}

// Stream opens a read stream for the part content.
// The caller is responsible for closing the returned ReadCloser.
func (p *Part) Stream() (io.ReadCloser, error) {
	if p == nil {
		return nil, ErrPartNotFound
	}
	if p.writeOnly {
		return nil, ErrWriteOnlyPart
	}
	if p.pkg != nil {
		p.pkg.mu.RLock()
		defer p.pkg.mu.RUnlock()
		if p.pkg.closed {
			return nil, ErrPackageClosed
		}
	}
	if p.archiveFile != nil {
		stream, err := p.archiveFile.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open part %s: %w", p.URI.String(), err)
		}
		if p.pkg.readerOptions.maxPartExpandedBytes != 0 {
			stream = &boundedReadCloser{
				stream:    stream,
				remaining: p.pkg.readerOptions.maxPartExpandedBytes,
				label:     fmt.Sprintf("part %s exceeds the expanded-size limit", p.URI.String()),
			}
		}
		return stream, nil
	}
	return io.NopCloser(bytes.NewReader(p.content)), nil
}

// ReadAllBytes reads the whole content of the part as bytes.
func (p *Part) ReadAllBytes() ([]byte, error) {
	stream, err := p.Stream()
	if err != nil {
		return nil, err
	}
	result, readErr := io.ReadAll(stream)
	closeErr := stream.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return result, nil
}

// ReadAllText reads the content of the part as UTF-8 text.
func (p *Part) ReadAllText() (string, error) {
	content, err := p.ReadAllBytes()
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// SupplementaryRelationship represents a relationship between a spec and its supplementary part.
type SupplementaryRelationship struct {
	Spec          *Part
	Supplementary *Part
}

// endregion

// region Packaging

// Packaging provides methods to open and create AAS packages.
type Packaging struct{}

// NewPackaging creates a new Packaging instance.
func NewPackaging() *Packaging {
	return &Packaging{}
}

// Create creates a new AAS package at the given path.
// Returns a PackageReadWrite instance for writing to the package.
func (p *Packaging) Create(path string) (*PackageReadWrite, error) {
	// Verify the path is writable by attempting to create/open the file
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, fmt.Errorf("directory does not exist: %s", dir)
	}

	// Try to create the file to verify it's writable
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("failed to create file: %w", err)
	}
	f.Close()

	pkg := newPackageBase(path, nil)
	pkg.readWrite = true

	// Create the origin part
	originURI, _ := url.Parse("/aasx/aasx-origin")
	originContent := []byte("Intentionally empty.")
	pkg.parts[normalizeURI(originURI)] = &Part{
		URI:         originURI,
		ContentType: "text/plain",
		content:     originContent,
		pkg:         pkg,
	}
	pkg.originURI = normalizeURI(originURI)

	// Create relationship from root to origin
	pkg.addRelationship("", originURI.String(), RelationTypeAasxOrigin)

	result := &PackageReadWrite{
		PackageRead: PackageRead{
			Path: path,
			base: pkg,
		},
	}

	specs, err := result.Specs()
	Ensure(err == nil, "Specs must be readable in a new package.")
	Ensure(len(specs) == 0, "Specs must be empty in a new package.")

	supplementaries, err := result.SupplementaryRelationships()
	Ensure(err == nil, "Supplementaries must be readable in a new package.")
	Ensure(
		len(supplementaries) == 0,
		"There must be no supplementary relationships in a new package.")

	thumbnail, err := result.Thumbnail()
	Ensure(err == nil, "Thumbnail must be readable in a new package.")
	Ensure(thumbnail == nil, "There must be no thumbnail in a new package.")

	return result, nil
}

// CreateInStream creates a new AAS package in the given stream.
// Returns a PackageReadWrite instance for writing to the package.
func (p *Packaging) CreateInStream(stream io.ReadWriteSeeker) (*PackageReadWrite, error) {
	pkg := newPackageBase("", stream)
	pkg.readWrite = true

	// Create the origin part
	originURI, _ := url.Parse("/aasx/aasx-origin")
	originContent := []byte("Intentionally empty.")
	pkg.parts[normalizeURI(originURI)] = &Part{
		URI:         originURI,
		ContentType: "text/plain",
		content:     originContent,
		pkg:         pkg,
	}
	pkg.originURI = normalizeURI(originURI)

	// Create relationship from root to origin
	pkg.addRelationship("", originURI.String(), RelationTypeAasxOrigin)

	result := &PackageReadWrite{
		PackageRead: PackageRead{
			Path: "",
			base: pkg,
		},
	}

	specs, err := result.Specs()
	Ensure(err == nil, "Specs must be readable in a new package.")
	Ensure(len(specs) == 0, "Specs must be empty in a new package.")

	supplementaries, err := result.SupplementaryRelationships()
	Ensure(err == nil, "Supplementaries must be readable in a new package.")
	Ensure(
		len(supplementaries) == 0,
		"There must be no supplementary relationships in a new package.")

	thumbnail, err := result.Thumbnail()
	Ensure(err == nil, "Thumbnail must be readable in a new package.")
	Ensure(thumbnail == nil, "There must be no thumbnail in a new package.")

	return result, nil
}

// OpenRead opens an AAS package at the given path for lazy reading.
// The package owns the opened file and releases it in PackageRead.Close.
func (p *Packaging) OpenRead(path string, options ...ReaderOption) (*PackageRead, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to stat file: %w", err)
	}

	result, err := p.openReadFromReaderAt(file, info.Size(), path, file, options)
	if err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, fmt.Errorf("%w; additionally failed to close file: %v", err, closeErr)
		}
		return nil, err
	}
	Ensure(result.Path == path, "The Path property of the package must match the input path.")
	return result, nil
}

// OpenReadFromStream opens an AAS package from the given stream for reading.
// Returns a PackageRead instance or an error.
func (p *Packaging) OpenReadFromStream(
	stream io.ReadSeeker,
	options ...ReaderOption,
) (*PackageRead, error) {
	if stream == nil {
		return nil, errors.New("stream must not be nil")
	}

	// Get the stream size
	size, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to seek stream: %w", err)
	}
	result, err := p.openReadFromReaderAt(
		&readSeekerAt{stream: stream}, size, "", nil, options)
	if err != nil {
		return nil, err
	}

	Ensure(
		result.Path == "",
		"The Path property of the package must be empty if reading from a stream.")

	return result, nil
}

// OpenReadFromReaderAt opens an AAS package lazily from a random-access reader.
// The caller retains ownership of reader and must keep it usable until Close.
func (p *Packaging) OpenReadFromReaderAt(
	reader io.ReaderAt,
	size int64,
	options ...ReaderOption,
) (*PackageRead, error) {
	return p.openReadFromReaderAt(reader, size, "", nil, options)
}

func (p *Packaging) openReadFromReaderAt(
	reader io.ReaderAt,
	size int64,
	path string,
	ownedCloser io.Closer,
	options []ReaderOption,
) (*PackageRead, error) {
	if reader == nil {
		return nil, errors.New("reader must not be nil")
	}
	if size < 0 {
		return nil, fmt.Errorf("reader size must not be negative: %d", size)
	}

	pkg, err := p.openPackage(reader, size, path, true, applyReaderOptions(options))
	if err != nil {
		return nil, err
	}
	pkg.readWrite = false
	pkg.ownedCloser = ownedCloser
	return &PackageRead{Path: path, base: pkg}, nil
}

// OpenReadWrite opens an AAS package at the given path for read/write.
// Returns a PackageReadWrite instance or an error.
func (p *Packaging) OpenReadWrite(path string) (*PackageReadWrite, error) {
	// Open and read the zip file
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	reader := bytes.NewReader(data)
	pkg, err := p.openFromReader(reader, int64(len(data)), path)
	if err != nil {
		return nil, err
	}
	pkg.readWrite = true

	result := &PackageReadWrite{
		PackageRead: PackageRead{
			Path: path,
			base: pkg,
		},
	}

	Ensure(result.Path == path, "The Path property of the package must match the input path.")

	return result, nil
}

// OpenReadWriteFromStream opens an AAS package from the given stream for read/write.
// Returns a PackageReadWrite instance or an error.
func (p *Packaging) OpenReadWriteFromStream(
	stream io.ReadWriteSeeker,
) (*PackageReadWrite, error) {
	// Get the stream size
	size, err := stream.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("failed to seek stream: %w", err)
	}
	if _, err := stream.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to reset stream: %w", err)
	}

	// Read all data into memory for zip.NewReader
	data := make([]byte, size)
	if _, err := io.ReadFull(stream, data); err != nil {
		return nil, fmt.Errorf("failed to read stream: %w", err)
	}

	reader := bytes.NewReader(data)
	pkg, err := p.openFromReader(reader, size, "")
	if err != nil {
		return nil, err
	}
	pkg.readWrite = true
	pkg.stream = stream

	result := &PackageReadWrite{
		PackageRead: PackageRead{
			Path: "",
			base: pkg,
		},
	}

	Ensure(
		result.Path == "",
		"The Path property of the package must be empty if read/writing to a stream.")

	return result, nil
}

// openFromReader materializes an OPC package for the mutable compatibility API.
func (p *Packaging) openFromReader(
	reader io.ReaderAt,
	size int64,
	path string,
) (*packageBase, error) {
	return p.openPackage(reader, size, path, false, readerOptions{})
}

func (p *Packaging) openPackage(
	reader io.ReaderAt,
	size int64,
	path string,
	lazy bool,
	options readerOptions,
) (*packageBase, error) {
	zipReader, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	pkg := newPackageBase(path, nil)
	pkg.archiveReader = zipReader
	pkg.readerOptions = options

	var partCount uint64
	var totalExpanded uint64
	for _, file := range zipReader.File {
		if strings.HasSuffix(file.Name, "/") {
			continue
		}
		partCount++
		if options.maxPartCount != 0 && partCount > options.maxPartCount {
			return nil, fmt.Errorf(
				"%w: package has more than %d parts",
				ErrReaderLimitExceeded,
				options.maxPartCount,
			)
		}
		if isOPCMetadataPath(file.Name) {
			continue
		}
		if options.maxPartExpandedBytes != 0 &&
			file.UncompressedSize64 > options.maxPartExpandedBytes {
			return nil, fmt.Errorf(
				"%w: part %s is %d bytes, maximum is %d",
				ErrReaderLimitExceeded,
				file.Name,
				file.UncompressedSize64,
				options.maxPartExpandedBytes,
			)
		}
		if totalExpanded > ^uint64(0)-file.UncompressedSize64 {
			return nil, fmt.Errorf("%w: total expanded size overflows uint64", ErrReaderLimitExceeded)
		}
		totalExpanded += file.UncompressedSize64
		if options.maxTotalExpandedBytes != 0 &&
			totalExpanded > options.maxTotalExpandedBytes {
			return nil, fmt.Errorf(
				"%w: total expanded size is more than %d bytes",
				ErrReaderLimitExceeded,
				options.maxTotalExpandedBytes,
			)
		}
	}

	var metadataBytes uint64
	readMetadata := func(file *zip.File, label string) ([]byte, error) {
		max := options.maxOPCMetadataBytes
		remaining := uint64(0)
		limited := max != 0
		if limited {
			if metadataBytes > max {
				return nil, fmt.Errorf("%w: OPC metadata exceeds %d bytes", ErrReaderLimitExceeded, max)
			}
			remaining = max - metadataBytes
		}
		data, readErr := readZipFile(archiveZipFile{file: file}, remaining, limited, label)
		if readErr != nil {
			return nil, readErr
		}
		metadataBytes += uint64(len(data))
		return data, nil
	}

	// Read [Content_Types].xml
	contentTypes := make(map[string]string) // path -> contentType
	defaultTypes := make(map[string]string) // extension -> contentType
	for _, file := range zipReader.File {
		if file.Name == "[Content_Types].xml" {
			data, err := readMetadata(file, "OPC content types")
			if err != nil {
				return nil, fmt.Errorf("failed to read content types: %w", err)
			}

			var ct contentTypesXML
			if err := xml.Unmarshal(data, &ct); err != nil {
				return nil, fmt.Errorf("failed to parse content types: %w", err)
			}

			for _, d := range ct.Defaults {
				defaultTypes[strings.ToLower(d.Extension)] = d.ContentType
			}
			for _, o := range ct.Overrides {
				contentTypes[normalizePathForMap(o.PartName)] = o.ContentType
			}
			break
		}
	}

	// Read all relationships files
	for _, file := range zipReader.File {
		relsPath := strings.ReplaceAll(file.Name, "\\", "/")
		if strings.Contains(relsPath, "_rels/") && strings.HasSuffix(relsPath, ".rels") {
			data, err := readMetadata(file, fmt.Sprintf("relationship part %s", file.Name))
			if err != nil {
				return nil, fmt.Errorf("failed to read rels file %s: %w", file.Name, err)
			}

			var rels relationshipsXML
			if err := xml.Unmarshal(data, &rels); err != nil {
				return nil, fmt.Errorf("failed to parse rels file %s: %w", file.Name, err)
			}

			// Determine source path from rels file path
			sourcePath := getSourcePathFromRelsPath(relsPath)

			for _, rel := range rels.Relationships {
				targetPath := resolveRelativeURI(sourcePath, rel.Target)
				pkg.addRelationshipWithID(sourcePath, targetPath, rel.Type, rel.ID)

				// Check if this is the origin relationship
				if relationshipTypesEqual(rel.Type, RelationTypeAasxOrigin) && sourcePath == "" {
					pkg.originURI = normalizePathForMap(targetPath)
				}
			}
		}
	}

	// Check that we found an origin
	if pkg.originURI == "" {
		return nil, ErrNoOriginPart
	}

	// Read all parts (excluding rels files and content types)
	for _, file := range zipReader.File {
		zipPath := strings.ReplaceAll(file.Name, "\\", "/")
		if zipPath == "[Content_Types].xml" ||
			strings.Contains(zipPath, "_rels/") ||
			strings.HasSuffix(zipPath, "/") {
			continue
		}

		partPath := "/" + strings.TrimPrefix(zipPath, "/")
		normalizedPath := normalizePathForMap(partPath)

		// Determine content type
		contentType := ""
		if ct, ok := contentTypes[normalizedPath]; ok {
			contentType = ct
		} else {
			ext := strings.TrimPrefix(pathpkg.Ext(zipPath), ".")
			if ct, ok := defaultTypes[strings.ToLower(ext)]; ok {
				contentType = ct
			} else {
				contentType = "application/octet-stream"
			}
		}

		partURI, _ := url.Parse(partPath)
		part := &Part{
			URI:         partURI,
			ContentType: contentType,
			pkg:         pkg,
		}
		if lazy {
			part.archiveFile = file
		} else {
			data, readErr := readZipFile(
				archiveZipFile{file: file}, 0, false, fmt.Sprintf("part %s", file.Name))
			if readErr != nil {
				return nil, fmt.Errorf("failed to read part %s: %w", file.Name, readErr)
			}
			part.content = data
		}
		pkg.parts[normalizedPath] = part
	}

	return pkg, nil
}

// endregion

// region PackageBase (internal)

// packageBase holds the internal state of a package
type packageBase struct {
	path          string
	stream        io.ReadWriteSeeker
	parts         map[string]*Part          // normalized path -> Part
	relationships map[string][]relationship // source path -> relationships
	originURI     string                    // normalized path to origin part
	readWrite     bool
	nextRelID     int
	archiveReader *zip.Reader
	readerOptions readerOptions
	ownedCloser   io.Closer
	closed        bool
	closeErr      error
	mu            sync.RWMutex
}

func newPackageBase(path string, stream io.ReadWriteSeeker) *packageBase {
	return &packageBase{
		path:          path,
		stream:        stream,
		parts:         make(map[string]*Part),
		relationships: make(map[string][]relationship),
		nextRelID:     1,
	}
}

func (pkg *packageBase) close() error {
	pkg.mu.Lock()
	defer pkg.mu.Unlock()
	if pkg.closed {
		return pkg.closeErr
	}
	pkg.closed = true
	if pkg.ownedCloser != nil {
		pkg.closeErr = pkg.ownedCloser.Close()
		pkg.ownedCloser = nil
	}
	return pkg.closeErr
}

func (pkg *packageBase) addRelationship(sourcePath, targetPath, relType string) string {
	id := fmt.Sprintf("R%08x", pkg.nextRelID)
	pkg.nextRelID++
	return pkg.addRelationshipWithID(sourcePath, targetPath, relType, id)
}

func (pkg *packageBase) addRelationshipWithID(sourcePath, targetPath, relType, id string) string {
	return pkg.addRelationshipWithIDAndMode(sourcePath, targetPath, relType, id, "Internal")
}

func (pkg *packageBase) addRelationshipWithIDAndMode(
	sourcePath, targetPath, relType, id, targetMode string) string {
	normalizedSource := normalizePathForMap(sourcePath)
	rel := relationship{
		id:         id,
		relType:    relType,
		target:     targetPath,
		targetMode: targetMode,
	}
	pkg.relationships[normalizedSource] = append(pkg.relationships[normalizedSource], rel)
	return id
}

func (pkg *packageBase) removeRelationship(sourcePath, targetPath, relType string) {
	normalizedSource := normalizePathForMap(sourcePath)
	normalizedTarget := normalizePathForMap(targetPath)

	rels := pkg.relationships[normalizedSource]
	var newRels []relationship
	for _, rel := range rels {
		if !(normalizePathForMap(rel.target) == normalizedTarget &&
			relationshipTypesEqual(rel.relType, relType)) {
			newRels = append(newRels, rel)
		}
	}
	pkg.relationships[normalizedSource] = newRels
}

func (pkg *packageBase) getRelationshipsByType(sourcePath, relType string) []relationship {
	normalizedSource := normalizePathForMap(sourcePath)
	var result []relationship
	for _, rel := range pkg.relationships[normalizedSource] {
		if relationshipTypesEqual(rel.relType, relType) {
			result = append(result, rel)
		}
	}
	return result
}

func (pkg *packageBase) hasRelationship(sourcePath, targetPath, relType string) bool {
	normalizedSource := normalizePathForMap(sourcePath)
	normalizedTarget := normalizePathForMap(targetPath)

	for _, rel := range pkg.relationships[normalizedSource] {
		if relationshipTypesEqual(rel.relType, relType) &&
			normalizePathForMap(rel.target) == normalizedTarget {
			return true
		}
	}
	return false
}

// endregion

// region PackageRead

// PackageRead provides read-only access to an AAS package.
type PackageRead struct {
	// Path is the file path associated with the package.
	// Empty if the package was opened from a stream.
	Path string

	base *packageBase
}

// Close closes the package and releases all resources.
func (p *PackageRead) Close() error {
	if p == nil || p.base == nil {
		return nil
	}
	err := p.base.close()
	p.base = nil
	return err
}

// Specs returns all AAS spec parts contained in the package.
func (p *PackageRead) Specs() ([]*Part, error) {
	p.base.mu.RLock()
	defer p.base.mu.RUnlock()

	var result []*Part
	rels := p.base.getRelationshipsByType(p.base.originURI, RelationTypeAasxSpec)

	for _, rel := range rels {
		normalizedTarget := normalizePathForMap(rel.target)
		if part, ok := p.base.parts[normalizedTarget]; ok {
			result = append(result, part)
		}
	}

	return result, nil
}

// SpecsByContentType returns AAS specs grouped by their MIME content type.
func (p *PackageRead) SpecsByContentType() (map[string][]*Part, error) {
	specs, err := p.Specs()
	if err != nil {
		return nil, err
	}

	result := make(map[string][]*Part)
	for _, spec := range specs {
		result[spec.ContentType] = append(result[spec.ContentType], spec)
	}

	// Sort specs within each content type by URI
	for ct := range result {
		sort.Slice(result[ct], func(i, j int) bool {
			return result[ct][i].URI.String() < result[ct][j].URI.String()
		})
	}

	for contentType, specs := range result {
		for _, spec := range specs {
			Ensure(spec.ContentType == contentType, "The content type of spec must match its group.")
		}
		Ensure(len(specs) > 0, "Every entry in the result must contain non-empty specs.")
	}

	return result, nil
}

// IsSpec checks whether the given part is related to the origin of the package as a spec.
func (p *PackageRead) IsSpec(part *Part) (bool, error) {
	p.base.mu.RLock()
	defer p.base.mu.RUnlock()

	return p.base.hasRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec), nil
}

// SupplementariesFor returns all supplementary parts for the given spec.
func (p *PackageRead) SupplementariesFor(spec *Part) ([]*Part, error) {
	p.base.mu.RLock()
	defer p.base.mu.RUnlock()

	var result []*Part
	rels := p.base.getRelationshipsByType(spec.URI.String(), RelationTypeAasxSupplementary)

	for _, rel := range rels {
		normalizedTarget := normalizePathForMap(rel.target)
		if part, ok := p.base.parts[normalizedTarget]; ok {
			result = append(result, part)
		} else {
			return nil, fmt.Errorf("supplementary part %s not found", rel.target)
		}
	}

	return result, nil
}

// SupplementaryRelationships iterates over all supplementary relationships from all specs.
func (p *PackageRead) SupplementaryRelationships() ([]*SupplementaryRelationship, error) {
	specs, err := p.Specs()
	if err != nil {
		return nil, err
	}

	var result []*SupplementaryRelationship
	for _, spec := range specs {
		suppls, err := p.SupplementariesFor(spec)
		if err != nil {
			return nil, err
		}
		for _, suppl := range suppls {
			result = append(result, &SupplementaryRelationship{
				Spec:          spec,
				Supplementary: suppl,
			})
		}
	}

	return result, nil
}

// FindPart tries to find the package part with the given URI.
// Returns nil if the part does not exist.
func (p *PackageRead) FindPart(uri *url.URL) (*Part, error) {
	p.base.mu.RLock()
	defer p.base.mu.RUnlock()

	normalizedURI := normalizeURI(uri)
	if part, ok := p.base.parts[normalizedURI]; ok {
		return part, nil
	}
	return nil, nil
}

// MustPart retrieves the package part with the given URI.
// Returns an error if the part does not exist.
func (p *PackageRead) MustPart(uri *url.URL) (*Part, error) {
	part, err := p.FindPart(uri)
	if err != nil {
		return nil, err
	}
	if part == nil {
		return nil, fmt.Errorf("%w: %s", ErrPartNotFound, uri.String())
	}
	return part, nil
}

// Thumbnail retrieves the thumbnail from the AAS package.
// Returns nil if no thumbnail exists in the package.
func (p *PackageRead) Thumbnail() (*Part, error) {
	p.base.mu.RLock()
	defer p.base.mu.RUnlock()

	// Look for thumbnail relationship from root
	rels := p.base.getRelationshipsByType("", RelationTypeThumbnail)

	if len(rels) > 0 {
		rel := rels[0]
		normalizedTarget := normalizePathForMap(rel.target)
		if part, ok := p.base.parts[normalizedTarget]; ok {
			return part, nil
		}
		return nil, fmt.Errorf("thumbnail relationship exists but part not found: %s", rel.target)
	}

	return nil, nil
}

// endregion

// region PackageWriter

// PackageWriter writes a new AAS package incrementally. It is append-only and
// retains only part and relationship metadata in memory.
type PackageWriter struct {
	archiveWriter *zip.Writer
	base          *packageBase
	failure       error
	closed        bool
	closeErr      error
	mu            sync.Mutex
}

// CreateWriter creates a bounded-memory, append-only package writer.
// The caller retains ownership of writer.
func (p *Packaging) CreateWriter(writer io.Writer) (*PackageWriter, error) {
	if writer == nil {
		return nil, errors.New("writer must not be nil")
	}

	result := &PackageWriter{
		archiveWriter: zip.NewWriter(writer),
		base:          newPackageBase("", nil),
	}

	originURI, _ := url.Parse("/aasx/aasx-origin")
	origin, err := result.putPartFromStreamLocked(
		originURI,
		"text/plain",
		strings.NewReader("Intentionally empty."),
	)
	if err != nil {
		closeErr := result.archiveWriter.Close()
		return nil, combineErrors(err, closeErr)
	}
	result.base.originURI = normalizeURI(origin.URI)
	result.base.addRelationship("", origin.URI.String(), RelationTypeAasxOrigin)
	return result, nil
}

// PutPartFromStream writes one complete part before returning. A failed input
// or output poisons the writer because an append-only ZIP can not roll back it.
func (writer *PackageWriter) PutPartFromStream(
	uri *url.URL,
	contentType string,
	stream io.Reader,
) (*Part, error) {
	if writer == nil {
		return nil, ErrWriterClosed
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.stateErrorLocked(); err != nil {
		return nil, err
	}
	if stream == nil {
		return nil, errors.New("part stream must not be nil")
	}
	return writer.putPartFromStreamLocked(uri, contentType, stream)
}

func (writer *PackageWriter) putPartFromStreamLocked(
	uri *url.URL,
	contentType string,
	stream io.Reader,
) (*Part, error) {
	partURI, normalizedURI, zipPath, err := writer.validateNewPartLocked(uri)
	if err != nil {
		return nil, err
	}

	entry, err := writer.archiveWriter.Create(zipPath)
	if err != nil {
		return nil, writer.failLocked(fmt.Errorf("failed to create part %s: %w", zipPath, err))
	}
	buffer := make([]byte, streamCopyBufferSize)
	if _, err := io.CopyBuffer(entry, struct{ io.Reader }{stream}, buffer); err != nil {
		return nil, writer.failLocked(fmt.Errorf("failed to copy part %s: %w", zipPath, err))
	}

	part := &Part{
		URI:         partURI,
		ContentType: contentType,
		writeOnly:   true,
	}
	writer.base.parts[normalizedURI] = part
	return part, nil
}

func (writer *PackageWriter) validateNewPartLocked(
	uri *url.URL,
) (*url.URL, string, string, error) {
	if uri == nil || uri.Path == "" {
		return nil, "", "", errors.New("part URI must not be empty")
	}
	if uri.Scheme != "" || uri.Host != "" || uri.RawQuery != "" || uri.Fragment != "" {
		return nil, "", "", fmt.Errorf("part URI must be an internal path: %s", uri.String())
	}

	partPath := normalizePathForURI(uri.Path)
	zipPath := strings.TrimPrefix(partPath, "/")
	if partPath == "/" || strings.HasSuffix(uri.Path, "/") {
		return nil, "", "", fmt.Errorf("part URI must identify a file: %s", uri.String())
	}
	if isOPCMetadataPath(zipPath) {
		return nil, "", "", fmt.Errorf("part URI is reserved for OPC metadata: %s", partPath)
	}

	partURI, err := url.Parse(partPath)
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid part URI %s: %w", partPath, err)
	}
	normalizedURI := normalizeURI(partURI)
	if _, exists := writer.base.parts[normalizedURI]; exists {
		return nil, "", "", fmt.Errorf("part already exists: %s", partPath)
	}
	return partURI, normalizedURI, zipPath, nil
}

// MakeSpec relates part to the AASX origin as a specification part.
func (writer *PackageWriter) MakeSpec(part *Part) error {
	if writer == nil {
		return ErrWriterClosed
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.stateErrorLocked(); err != nil {
		return err
	}
	stored, err := writer.validatePartLocked(part)
	if err != nil {
		return err
	}
	if !writer.base.hasRelationship(
		writer.base.originURI, stored.URI.String(), RelationTypeAasxSpec) {
		writer.base.addRelationship(
			writer.base.originURI, stored.URI.String(), RelationTypeAasxSpec)
	}
	return nil
}

// RelateSupplementaryToSpec creates a supplementary relationship. Its argument
// order matches PackageReadWrite: supplementary first, specification second.
func (writer *PackageWriter) RelateSupplementaryToSpec(
	supplementary *Part,
	spec *Part,
) error {
	if writer == nil {
		return ErrWriterClosed
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.stateErrorLocked(); err != nil {
		return err
	}
	storedSupplementary, err := writer.validatePartLocked(supplementary)
	if err != nil {
		return err
	}
	storedSpec, err := writer.validatePartLocked(spec)
	if err != nil {
		return err
	}
	if !writer.base.hasRelationship(
		writer.base.originURI, storedSpec.URI.String(), RelationTypeAasxSpec) {
		return fmt.Errorf("part is not a specification: %s", storedSpec.URI.String())
	}
	if !writer.base.hasRelationship(
		storedSpec.URI.String(),
		storedSupplementary.URI.String(),
		RelationTypeAasxSupplementary,
	) {
		writer.base.addRelationship(
			storedSpec.URI.String(),
			storedSupplementary.URI.String(),
			RelationTypeAasxSupplementary,
		)
	}
	return nil
}

// SetThumbnail sets part as the package thumbnail.
func (writer *PackageWriter) SetThumbnail(part *Part) error {
	if writer == nil {
		return ErrWriterClosed
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if err := writer.stateErrorLocked(); err != nil {
		return err
	}
	stored, err := writer.validatePartLocked(part)
	if err != nil {
		return err
	}
	for _, rel := range writer.base.getRelationshipsByType("", RelationTypeThumbnail) {
		writer.base.removeRelationship("", rel.target, RelationTypeThumbnail)
	}
	writer.base.addRelationship("", stored.URI.String(), RelationTypeThumbnail)
	return nil
}

func (writer *PackageWriter) validatePartLocked(part *Part) (*Part, error) {
	if part == nil || !part.writeOnly || part.URI == nil {
		return nil, errors.New("part does not belong to this package writer")
	}
	stored, exists := writer.base.parts[normalizeURI(part.URI)]
	if !exists || stored != part {
		return nil, errors.New("part does not belong to this package writer")
	}
	return stored, nil
}

func (writer *PackageWriter) stateErrorLocked() error {
	if writer.closed {
		return ErrWriterClosed
	}
	return writer.failure
}

func (writer *PackageWriter) failLocked(err error) error {
	if writer.failure == nil {
		writer.failure = err
	}
	return writer.failure
}

// Close writes OPC metadata and finalizes the ZIP exactly once. It does not
// close the caller-owned destination.
func (writer *PackageWriter) Close() error {
	if writer == nil {
		return ErrWriterClosed
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed {
		return writer.closeErr
	}
	writer.closed = true

	result := writer.failure
	if result == nil {
		result = writer.writeMetadataLocked()
	}
	result = combineErrors(result, writer.archiveWriter.Close())
	writer.closeErr = result
	return result
}

func (writer *PackageWriter) writeMetadataLocked() error {
	contentTypes := buildContentTypes(writer.base.parts)
	if err := writer.writeXMLEntryLocked("[Content_Types].xml", contentTypes); err != nil {
		return fmt.Errorf("failed to write content types: %w", err)
	}

	sourcePaths := make([]string, 0, len(writer.base.relationships))
	for sourcePath, relationships := range writer.base.relationships {
		if len(relationships) != 0 {
			sourcePaths = append(sourcePaths, sourcePath)
		}
	}
	sort.Strings(sourcePaths)
	for _, sourcePath := range sourcePaths {
		rels := relationshipsXML{Xmlns: opcRelationshipNamespace}
		for _, rel := range writer.base.relationships[sourcePath] {
			rels.Relationships = append(rels.Relationships, relationshipXML{
				ID:         rel.id,
				Type:       rel.relType,
				Target:     rel.target,
				TargetMode: rel.targetMode,
			})
		}
		entryPath := strings.TrimPrefix(getRelsPath(sourcePath), "/")
		if err := writer.writeXMLEntryLocked(entryPath, rels); err != nil {
			return fmt.Errorf("failed to write relationships %s: %w", entryPath, err)
		}
	}
	return nil
}

func (writer *PackageWriter) writeXMLEntryLocked(name string, value interface{}) error {
	entry, err := writer.archiveWriter.Create(name)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(entry, xml.Header); err != nil {
		return err
	}
	encoder := xml.NewEncoder(entry)
	encoder.Indent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return err
	}
	return encoder.Flush()
}

func combineErrors(primary error, additional error) error {
	if primary == nil {
		return additional
	}
	if additional == nil {
		return primary
	}
	return fmt.Errorf("%w; additionally: %v", primary, additional)
}

// endregion

// region PackageReadWrite

// PackageReadWrite provides read and write access to an AAS package.
// It embeds PackageRead for read functionality.
type PackageReadWrite struct {
	PackageRead
}

// PutPart writes content to the package as a package part with the given content type.
// This function needs to be used to put any content into the package.
// You have to introduce the relations by calling RelateSupplementaryToSpec, MakeSpec, etc.
func (p *PackageReadWrite) PutPart(
	uri *url.URL,
	contentType string,
	content []byte,
) (*Part, error) {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	normalizedURI := normalizeURI(uri)

	// Create new part or update existing
	contentCopy := make([]byte, len(content))
	copy(contentCopy, content)

	part := &Part{
		URI:         uri,
		ContentType: contentType,
		content:     contentCopy,
		pkg:         p.base,
	}

	p.base.parts[normalizedURI] = part

	stored := p.base.parts[normalizedURI]
	Ensure(stored != nil, "The part should be included in the package.")
	if stored != nil {
		Ensure(
			bytes.Equal(stored.content, content),
			"Input content and re-read content must coincide on put.")
	}

	return part, nil
}

// PutPartFromStream writes content from a stream to the package as a package part.
func (p *PackageReadWrite) PutPartFromStream(
	uri *url.URL,
	contentType string,
	stream io.Reader,
) (*Part, error) {
	content, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to read stream: %w", err)
	}
	return p.PutPart(uri, contentType, content)
}

// DeletePart removes a part from the package.
func (p *PackageReadWrite) DeletePart(part *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	normalizedURI := normalizeURI(part.URI)
	delete(p.base.parts, normalizedURI)

	_, exists := p.base.parts[normalizedURI]
	Ensure(!exists, "The part should not exist in the package anymore.")

	return nil
}

// MakeSpec relates the given part to the origin as a spec.
func (p *PackageReadWrite) MakeSpec(part *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	// Check if relationship already exists
	if p.base.hasRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec) {
		return nil
	}

	p.base.addRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec)

	listed := p.base.hasRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec)
	if listed {
		_, exists := p.base.parts[normalizeURI(part.URI)]
		listed = exists
	}
	Ensure(listed, "Spec must be listed.")

	isSpec := p.base.hasRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec)
	Ensure(isSpec, "The part fulfills the spec property.")
	return nil
}

// UnmakeSpec removes the spec relationship for the given part.
func (p *PackageReadWrite) UnmakeSpec(part *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	isSpec := p.base.hasRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec)
	Require(isSpec, "The part fulfills the spec property.")

	oldSpecURISet := make(map[string]struct{})
	for _, rel := range p.base.getRelationshipsByType(
		p.base.originURI,
		RelationTypeAasxSpec,
	) {
		normalizedTarget := normalizePathForMap(rel.target)
		if _, ok := p.base.parts[normalizedTarget]; ok {
			oldSpecURISet[normalizedTarget] = struct{}{}
		}
	}

	p.base.removeRelationship(p.base.originURI, part.URI.String(), RelationTypeAasxSpec)

	newSpecURISet := make(map[string]struct{})
	for _, rel := range p.base.getRelationshipsByType(
		p.base.originURI,
		RelationTypeAasxSpec,
	) {
		normalizedTarget := normalizePathForMap(rel.target)
		if _, ok := p.base.parts[normalizedTarget]; ok {
			newSpecURISet[normalizedTarget] = struct{}{}
		}
	}

	Ensure(func() bool {
		_, exists := newSpecURISet[normalizeURI(part.URI)]
		return !exists
	}(), "The spec must not be listed in the Specs().")

	_, existed := oldSpecURISet[normalizeURI(part.URI)]
	if existed {
		Ensure(len(newSpecURISet) == len(oldSpecURISet)-1, "No other spec has been removed.")
		removed := 0
		for uri := range oldSpecURISet {
			if _, ok := newSpecURISet[uri]; !ok {
				removed++
				Ensure(uri == normalizeURI(part.URI), "No other spec has been removed.")
			}
		}
		Ensure(removed == 1, "No other spec has been removed.")
	}
	return nil
}

// RelateSupplementaryToSpec creates a supplementary relationship between a supplementary part
// and a spec.
func (p *PackageReadWrite) RelateSupplementaryToSpec(
	supplementary *Part,
	spec *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	isSpec := p.base.hasRelationship(p.base.originURI, spec.URI.String(), RelationTypeAasxSpec)
	Require(isSpec, "The part fulfills the spec property.")

	// Check if relationship already exists
	if p.base.hasRelationship(
		spec.URI.String(),
		supplementary.URI.String(),
		RelationTypeAasxSupplementary,
	) {
		return nil
	}

	p.base.addRelationship(
		spec.URI.String(),
		supplementary.URI.String(),
		RelationTypeAasxSupplementary)

	relExists := p.base.hasRelationship(
		spec.URI.String(),
		supplementary.URI.String(),
		RelationTypeAasxSupplementary)
	if relExists {
		_, exists := p.base.parts[normalizeURI(supplementary.URI)]
		relExists = exists
	}
	Ensure(relExists, "The supplementary must be listed.")
	return nil
}

// UnrelateSupplementaryFromSpec removes the supplementary relationship.
func (p *PackageReadWrite) UnrelateSupplementaryFromSpec(supplementary *Part, spec *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	isSpec := p.base.hasRelationship(p.base.originURI, spec.URI.String(), RelationTypeAasxSpec)
	Require(isSpec, "The part fulfills the spec property.")

	oldSupplURISet := make(map[string]struct{})
	for _, rel := range p.base.getRelationshipsByType(
		spec.URI.String(),
		RelationTypeAasxSupplementary,
	) {
		normalizedTarget := normalizePathForMap(rel.target)
		if _, ok := p.base.parts[normalizedTarget]; ok {
			oldSupplURISet[normalizedTarget] = struct{}{}
		}
	}

	p.base.removeRelationship(
		spec.URI.String(),
		supplementary.URI.String(),
		RelationTypeAasxSupplementary)

	newSupplURISet := make(map[string]struct{})
	for _, rel := range p.base.getRelationshipsByType(
		spec.URI.String(),
		RelationTypeAasxSupplementary,
	) {
		normalizedTarget := normalizePathForMap(rel.target)
		if _, ok := p.base.parts[normalizedTarget]; ok {
			newSupplURISet[normalizedTarget] = struct{}{}
		}
	}

	Ensure(func() bool {
		_, exists := newSupplURISet[normalizeURI(supplementary.URI)]
		return !exists
	}(), "The supplementary file must not be listed in the Supplementaries().")

	_, existed := oldSupplURISet[normalizeURI(supplementary.URI)]
	if existed {
		Ensure(
			len(newSupplURISet) == len(oldSupplURISet)-1,
			"No other supplementary has been removed.")
		removed := 0
		for uri := range oldSupplURISet {
			if _, ok := newSupplURISet[uri]; !ok {
				removed++
				Ensure(
					uri == normalizeURI(supplementary.URI),
					"No other supplementary has been removed.")
			}
		}
		Ensure(removed == 1, "No other supplementary has been removed.")
	}
	return nil
}

// SetThumbnail sets the thumbnail of the package.
func (p *PackageReadWrite) SetThumbnail(part *Part) error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	// First remove any existing thumbnail relationship
	rels := p.base.getRelationshipsByType("", RelationTypeThumbnail)
	for _, rel := range rels {
		p.base.removeRelationship("", rel.target, RelationTypeThumbnail)
	}

	// Add new thumbnail relationship
	p.base.addRelationship("", part.URI.String(), RelationTypeThumbnail)

	thumbnailRels := p.base.getRelationshipsByType("", RelationTypeThumbnail)
	Ensure(len(thumbnailRels) > 0, "The thumbnail must be available.")
	if len(thumbnailRels) > 0 {
		normalizedTarget := normalizePathForMap(thumbnailRels[0].target)
		stored, exists := p.base.parts[normalizedTarget]
		Ensure(exists, "The thumbnail must point to the part.")
		if exists {
			Ensure(
				normalizeURI(stored.URI) == normalizeURI(part.URI),
				"The thumbnail must point to the part.")
		}
	}
	return nil
}

// UnsetThumbnail removes the thumbnail from the package.
func (p *PackageReadWrite) UnsetThumbnail() error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	rels := p.base.getRelationshipsByType("", RelationTypeThumbnail)
	for _, rel := range rels {
		p.base.removeRelationship("", rel.target, RelationTypeThumbnail)
	}

	remaining := p.base.getRelationshipsByType("", RelationTypeThumbnail)
	Ensure(len(remaining) == 0, "The thumbnail must not exist any more")
	return nil
}

// Flush writes all pending changes to the underlying stream or file.
func (p *PackageReadWrite) Flush() error {
	p.base.mu.Lock()
	defer p.base.mu.Unlock()

	// Create the zip content in memory
	var buf bytes.Buffer
	if err := p.writeToZip(&buf); err != nil {
		return err
	}

	// Write to file or stream
	if p.Path != "" {
		if err := os.WriteFile(p.Path, buf.Bytes(), 0644); err != nil {
			return fmt.Errorf("failed to write file: %w", err)
		}
	} else if p.base.stream != nil {
		if _, err := p.base.stream.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek stream: %w", err)
		}
		if _, err := p.base.stream.Write(buf.Bytes()); err != nil {
			return fmt.Errorf("failed to write stream: %w", err)
		}
	}

	return nil
}

// writeToZip writes the package to a zip writer
func (p *PackageReadWrite) writeToZip(w io.Writer) error {
	zw := zip.NewWriter(w)
	defer zw.Close()

	// Build content types
	ct := p.buildContentTypes()
	ctData, err := xml.MarshalIndent(ct, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal content types: %w", err)
	}
	ctData = append([]byte(xml.Header), ctData...)

	// Write [Content_Types].xml
	ctWriter, err := zw.Create("[Content_Types].xml")
	if err != nil {
		return fmt.Errorf("failed to create content types entry: %w", err)
	}
	if _, err := ctWriter.Write(ctData); err != nil {
		return fmt.Errorf("failed to write content types: %w", err)
	}

	// Write relationship files
	relsWritten := make(map[string]bool)
	for sourcePath, rels := range p.base.relationships {
		if len(rels) == 0 {
			continue
		}

		relsPath := getRelsPath(sourcePath)
		if relsWritten[relsPath] {
			continue
		}
		relsWritten[relsPath] = true

		relsXML := relationshipsXML{
			Xmlns: opcRelationshipNamespace,
		}
		for _, rel := range rels {
			relsXML.Relationships = append(relsXML.Relationships, relationshipXML{
				ID:         rel.id,
				Type:       rel.relType,
				Target:     rel.target,
				TargetMode: rel.targetMode,
			})
		}

		relsData, err := xml.MarshalIndent(relsXML, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal rels: %w", err)
		}
		relsData = append([]byte(xml.Header), relsData...)

		relsWriter, err := zw.Create(strings.TrimPrefix(relsPath, "/"))
		if err != nil {
			return fmt.Errorf("failed to create rels entry: %w", err)
		}
		if _, err := relsWriter.Write(relsData); err != nil {
			return fmt.Errorf("failed to write rels: %w", err)
		}
	}

	// Write all parts
	for _, part := range p.base.parts {
		partPath := strings.TrimPrefix(part.URI.String(), "/")
		partWriter, err := zw.Create(partPath)
		if err != nil {
			return fmt.Errorf("failed to create part entry %s: %w", partPath, err)
		}
		if _, err := partWriter.Write(part.content); err != nil {
			return fmt.Errorf("failed to write part %s: %w", partPath, err)
		}
	}

	return nil
}

// buildContentTypes builds the content types XML structure
func (p *PackageReadWrite) buildContentTypes() contentTypesXML {
	return buildContentTypes(p.base.parts)
}

func buildContentTypes(parts map[string]*Part) contentTypesXML {
	ct := contentTypesXML{
		Xmlns: opcContentTypesNamespace,
	}

	// Add default for rels files
	ct.Defaults = append(ct.Defaults, defaultType{
		Extension:   "rels",
		ContentType: "application/vnd.openxmlformats-package.relationships+xml",
	})

	// Collect extensions and their content types
	extMap := make(map[string]string)
	overrides := make(map[string]string)

	for _, part := range parts {
		partPath := part.URI.String()
		ext := strings.TrimPrefix(filepath.Ext(partPath), ".")
		ext = strings.ToLower(ext)

		if ext == "" {
			// No extension, need override
			overrides[partPath] = part.ContentType
		} else {
			if existingCT, ok := extMap[ext]; ok {
				// Extension already seen with different content type - need override
				if existingCT != part.ContentType {
					overrides[partPath] = part.ContentType
				}
			} else {
				extMap[ext] = part.ContentType
			}
		}
	}

	// Add default types for extensions
	for ext, contentType := range extMap {
		ct.Defaults = append(ct.Defaults, defaultType{
			Extension:   ext,
			ContentType: contentType,
		})
	}

	// Sort defaults for reproducibility
	sort.Slice(ct.Defaults, func(i, j int) bool {
		return ct.Defaults[i].Extension < ct.Defaults[j].Extension
	})

	// Add overrides
	for partPath, contentType := range overrides {
		ct.Overrides = append(ct.Overrides, overrideType{
			PartName:    partPath,
			ContentType: contentType,
		})
	}

	// Sort overrides for reproducibility
	sort.Slice(ct.Overrides, func(i, j int) bool {
		return ct.Overrides[i].PartName < ct.Overrides[j].PartName
	})

	return ct
}

// endregion

// region Helper Functions

func isOPCMetadataPath(zipPath string) bool {
	zipPath = strings.ReplaceAll(zipPath, "\\", "/")
	return zipPath == "[Content_Types].xml" ||
		(strings.Contains(zipPath, "_rels/") && strings.HasSuffix(zipPath, ".rels"))
}

// normalizeURI returns a normalized string representation of a URI for use as map key
func normalizeURI(uri *url.URL) string {
	if uri == nil {
		return ""
	}
	path := uri.Path
	if uri.String() != "" && !strings.HasPrefix(uri.String(), "/") {
		path = "/" + uri.String()
	}
	return normalizePathForMap(path)
}

// normalizePathForMap normalizes a path for use as a map key
func normalizePathForMap(path string) string {
	path = normalizePathForURI(path)
	if path == "" {
		return ""
	}

	return strings.ToLower(path)
}

// normalizePathForURI normalizes an OPC URI-like path while preserving case.
func normalizePathForURI(path string) string {
	if path == "" {
		return ""
	}

	path = strings.ReplaceAll(path, "\\", "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	path = pathpkg.Clean(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	if path == "/." {
		return "/"
	}

	return path
}

// normalizeRelationshipType normalizes relationship type aliases to preferred forms.
//
// We replace deprecated relationships prefixes (since V3.0.1 of
// the meta-model) with the newer relationship prefix. This allows us to support both the
// legacy AASX files as well as the new version of AASX files. In particular, we use
// the normalized form in the equality checks (see `relationshipTypesEqual`).
func normalizeRelationshipType(relType string) string {
	if strings.HasPrefix(relType, DeprecatedAasxRelationshipsPrefix) {
		suffix := strings.TrimPrefix(relType, DeprecatedAasxRelationshipsPrefix)
		return PreferredAasxRelationshipsPrefix + suffix
	}
	return relType
}

// relationshipTypesEqual checks the relationship types for equality based on their
// normalized forms (see `normalizeRelationshipType`). Namely, we want the equality
// to be agnostic to legacy and current relationship type prefixes, so we normalize them
// before equality comparison.
func relationshipTypesEqual(left, right string) bool {
	return normalizeRelationshipType(left) == normalizeRelationshipType(right)
}

// getSourcePathFromRelsPath extracts the source path from a .rels file path
// e.g., "_rels/.rels" -> "" (root)
// e.g., "aasx/_rels/aasx-origin.rels" -> "/aasx/aasx-origin"
func getSourcePathFromRelsPath(relsPath string) string {
	relsPath = strings.ReplaceAll(relsPath, "\\", "/")
	relsPath = strings.TrimPrefix(relsPath, "/")
	relsPath = pathpkg.Clean("/" + relsPath)

	if relsPath == "/_rels/.rels" {
		return "" // Root relationships
	}

	// Extract directory and filename
	dir := pathpkg.Dir(relsPath)
	base := pathpkg.Base(relsPath)

	// Remove .rels extension
	sourceName := strings.TrimSuffix(base, ".rels")

	// Remove _rels from path
	dir = strings.TrimSuffix(dir, "/_rels")

	if dir == "" || dir == "." || dir == "/" {
		return normalizePathForURI("/" + sourceName)
	}

	return normalizePathForURI(dir + "/" + sourceName)
}

// getRelsPath returns the path to the .rels file for a given source path
func getRelsPath(sourcePath string) string {
	sourcePath = normalizePathForURI(sourcePath)
	if sourcePath == "" {
		return "/_rels/.rels"
	}

	dir := pathpkg.Dir(sourcePath)
	base := pathpkg.Base(sourcePath)

	if dir == "/" || dir == "." {
		return "/_rels/" + base + ".rels"
	}

	return normalizePathForURI(dir + "/_rels/" + base + ".rels")
}

// resolveRelativeURI resolves a relative URI against a source path
func resolveRelativeURI(sourcePath, target string) string {
	target = strings.ReplaceAll(target, "\\", "/")
	if strings.HasPrefix(target, "/") {
		return normalizePathForURI(target)
	}

	// Relative path - resolve against source directory
	normalizedSource := normalizePathForURI(sourcePath)
	sourceDir := pathpkg.Dir(normalizedSource)
	if sourceDir == "" || sourceDir == "." {
		sourceDir = "/"
	}

	resolved := pathpkg.Join(sourceDir, target)
	if !strings.HasPrefix(resolved, "/") {
		resolved = "/" + resolved
	}

	return normalizePathForURI(resolved)
}

// endregion
