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
	ErrNoOriginPart  = errors.New("no origin part found")
	ErrPartNotFound  = errors.New("part not found")
	ErrInvalidFormat = errors.New("invalid package format")
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

// region Part

// Part represents a part of an AAS package.
type Part struct {
	// URI is the location of the part within the package.
	URI *url.URL

	// ContentType is the MIME type of the part.
	ContentType string

	// content holds the byte content of the part
	content []byte

	// pkg is a reference back to the package (for lazy loading if needed)
	pkg *packageBase
}

// Stream opens a read stream for the part content.
// The caller is responsible for closing the returned ReadCloser.
func (p *Part) Stream() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(p.content)), nil
}

// ReadAllBytes reads the whole content of the part as bytes.
func (p *Part) ReadAllBytes() ([]byte, error) {
	result := make([]byte, len(p.content))
	copy(result, p.content)
	return result, nil
}

// ReadAllText reads the content of the part as UTF-8 text.
func (p *Part) ReadAllText() (string, error) {
	return string(p.content), nil
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

// OpenRead opens an AAS package at the given path for reading.
// Returns a PackageRead instance or an error.
func (p *Packaging) OpenRead(path string) (*PackageRead, error) {
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
	pkg.readWrite = false

	result := &PackageRead{
		Path: path,
		base: pkg,
	}

	Ensure(result.Path == path, "The Path property of the package must match the input path.")

	return result, nil
}

// OpenReadFromStream opens an AAS package from the given stream for reading.
// Returns a PackageRead instance or an error.
func (p *Packaging) OpenReadFromStream(stream io.ReadSeeker) (*PackageRead, error) {
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
	pkg.readWrite = false

	result := &PackageRead{
		Path: "",
		base: pkg,
	}

	Ensure(
		result.Path == "",
		"The Path property of the package must be empty if reading from a stream.")

	return result, nil
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

// openFromReader opens an OPC package from a reader
func (p *Packaging) openFromReader(
	reader *bytes.Reader,
	size int64,
	path string,
) (*packageBase, error) {
	zipReader, err := zip.NewReader(reader, size)
	if err != nil {
		return nil, fmt.Errorf("failed to open zip: %w", err)
	}

	pkg := newPackageBase(path, nil)

	// Read [Content_Types].xml
	contentTypes := make(map[string]string) // path -> contentType
	defaultTypes := make(map[string]string) // extension -> contentType
	for _, file := range zipReader.File {
		if file.Name == "[Content_Types].xml" {
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open content types: %w", err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
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
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("failed to open rels file %s: %w", file.Name, err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
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

		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open part %s: %w", file.Name, err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read part %s: %w", file.Name, err)
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
		pkg.parts[normalizedPath] = &Part{
			URI:         partURI,
			ContentType: contentType,
			content:     data,
			pkg:         pkg,
		}
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
	// Clear references
	p.base = nil
	return nil
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

	for _, part := range p.base.parts {
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
