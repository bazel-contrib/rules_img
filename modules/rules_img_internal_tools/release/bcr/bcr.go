package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type registry struct {
	Mirrors        []string `json:"mirrors"`
	ModuleBasePath string   `json:"module_base_path,omitempty"`
}

type maintainer struct {
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	GitHub      string `json:"github,omitempty"`
	DoNotNotify *bool  `json:"do_not_notify,omitempty"`
}

type metadataInfo struct {
	Homepage       string            `json:"homepage"`
	Maintainers    []maintainer      `json:"maintainers"`
	Repository     []string          `json:"repository"`
	Versions       []string          `json:"versions"`
	YankedVersions map[string]string `json:"yanked_versions,omitempty"`
	Deprecated     string            `json:"deprecated,omitempty"`
}

type moduleVersionSource struct {
	Type        string            `json:"type,omitempty"`
	URL         string            `json:"url,omitempty"`
	Path        string            `json:"path,omitempty"`
	Integrity   string            `json:"integrity"`
	StripPrefix string            `json:"strip_prefix,omitempty"`
	Overlay     map[string]string `json:"overlay,omitempty"`
	Patches     map[string]string `json:"patches,omitempty"`
	PatchStrip  int               `json:"patch_strip,omitempty"`
	ArchiveType string            `json:"archive_type,omitempty"`
}

type addLocalModuleVersionReq struct {
	ModuleName             string `json:"module_name"`
	Version                string `json:"version"`
	SourcePath             string `json:"source_path"`
	OverrideSourceBasename string `json:"override_source_basename,omitempty"`
	MetadataTemplatePath   string `json:"metadata_template_path"`
	moduleVersionSource
}

type addRequests []addLocalModuleVersionReq

func (r *addRequests) String() string {
	return fmt.Sprintf("%v", *r)
}

func (r *addRequests) Set(value string) error {
	requestJSON, err := os.ReadFile(value)
	if err != nil {
		return err
	}
	var req addLocalModuleVersionReq
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return err
	}
	*r = append(*r, req)
	return nil
}

var requests addRequests

func main() {
	flag.Var(&requests, "add-local-module", "Path to a JSON manifest of a new module to add.")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "expected destination dir")
		os.Exit(1)
	}
	destination := flag.Arg(0)

	if len(requests) == 0 {
		fmt.Fprintln(os.Stderr, "Empty list of modules to add.")
		os.Exit(1)
	}
	if err := os.MkdirAll(destination, os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "Preparing BCR directory: %v\n", err)
	}
	if err := os.Mkdir(filepath.Join(destination, sourcesBasePath), os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "Preparing module base path: %v\n", err)
	}
	registryJSON := registry{
		Mirrors:        []string{},
		ModuleBasePath: sourcesBasePath,
	}
	buf := bytes.NewBuffer(nil)
	registryEncoder := json.NewEncoder(buf)
	registryEncoder.SetIndent("", "  ")
	if err := registryEncoder.Encode(registryJSON); err != nil {
		fmt.Fprintf(os.Stderr, "Encoding registry JSON: %v\n", err)
	}
	if err := os.WriteFile(filepath.Join(destination, "bazel_registry.json"), buf.Bytes(), os.ModePerm); err != nil {
		fmt.Fprintf(os.Stderr, "Writing registry JSON: %v\n", err)
	}

	moduleVersions := make(map[string]metadataInfo)
	for _, req := range requests {
		if err := createModuleVersion(destination, req); err != nil {
			fmt.Fprintf(os.Stderr, "Creating module version %s %s: %v\n", req.ModuleName, req.Version, err)
			os.Exit(1)
		}
		if _, ok := moduleVersions[req.ModuleName]; !ok {
			var metadata metadataInfo
			metadataTemplate, err := os.ReadFile(req.MetadataTemplatePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Reading metadata template %s: %v\n", req.MetadataTemplatePath, err)
				os.Exit(1)
			}
			if err := json.Unmarshal(metadataTemplate, &metadata); err != nil {
				fmt.Fprintf(os.Stderr, "Unmarshalling metadata template %s: %v\n", req.MetadataTemplatePath, err)
				os.Exit(1)
			}
			moduleVersions[req.ModuleName] = metadata
		}
		module := moduleVersions[req.ModuleName]
		module.Versions = append(module.Versions, req.Version)
		slices.Sort(module.Versions)
		moduleVersions[req.ModuleName] = module
	}
	// write metadata.json once per module
	buf.Reset()
	metadataEncoder := json.NewEncoder(buf)
	metadataEncoder.SetIndent("", "  ")
	for moduleName, metadata := range moduleVersions {
		metadataEncoder.Encode(metadata)
		metadataPath := filepath.Join(destination, "modules", moduleName, "metadata.json")
		if err := os.WriteFile(metadataPath, buf.Bytes(), os.ModePerm); err != nil {
			fmt.Fprintf(os.Stderr, "Writing metadata for module %s: %v\n", moduleName, err)
			os.Exit(1)
		}
		buf.Reset()
	}
}

func createModuleVersion(destination string, request addLocalModuleVersionReq) error {
	moduleDir := filepath.Join(destination, "modules", request.ModuleName)
	versionDir := filepath.Join(moduleDir, request.Version)
	moduleFilePath := filepath.Join(versionDir, "MODULE.bazel")
	sourceJSONPath := filepath.Join(versionDir, "source.json")

	integrity, err := sourceArchiveIntegrity(request.SourcePath)
	if err != nil {
		return err
	}
	request.Integrity = integrity

	request.Type = "local_path"
	sourceArchiveDir := filepath.Join(request.ModuleName, request.Version)
	basename := filepath.Base(request.SourcePath)
	if request.Path != "" {
		basename = filepath.Base(request.Path)
	}
	if request.OverrideSourceBasename != "" {
		basename = request.OverrideSourceBasename
	}
	// We want the extracted source to be in a directory named "src"
	// And also the raw archive next to it
	extractedPath := filepath.Join(sourceArchiveDir, "src")
	archivePath := filepath.Join(sourceArchiveDir, basename)
	request.Path = filepath.ToSlash(extractedPath)

	archiveFilepath := filepath.Join(destination, sourcesBasePath, archivePath)
	extractedFilepath := filepath.Join(destination, sourcesBasePath, extractedPath)

	if err := os.MkdirAll(versionDir, os.ModePerm); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(extractedFilepath), os.ModePerm); err != nil {
		return err
	}

	moduleFileContents, err := extractModuleFile(request.SourcePath, request.StripPrefix)
	if err != nil {
		return err
	}
	if err := os.WriteFile(moduleFilePath, moduleFileContents, os.ModePerm); err != nil {
		return err
	}

	sourceJSON, err := json.Marshal(request.moduleVersionSource)
	if err != nil {
		return err
	}
	if err := os.WriteFile(sourceJSONPath, sourceJSON, os.ModePerm); err != nil {
		return err
	}
	if err := copySourceArchive(request.SourcePath, archiveFilepath); err != nil {
		return err
	}
	return extractSourceArchive(request.SourcePath, extractedFilepath, request.StripPrefix)
}

func extractModuleFile(sourceArchive string, stripPrefix string) ([]byte, error) {
	pathInArchive := "MODULE.bazel"
	if stripPrefix != "" {
		pathInArchive = filepath.Join(stripPrefix, pathInArchive)
	}
	archive, err := os.Open(sourceArchive)
	if err != nil {
		return nil, err
	}
	defer archive.Close()

	var archiveReader io.ReadCloser
	if filepath.Ext(sourceArchive) == ".tar" {
		archiveReader = archive
	} else if filepath.Ext(sourceArchive) == ".gz" {
		archiveReader, err = gzip.NewReader(archive)
		if err != nil {
			return nil, err
		}
		defer archiveReader.Close()
	} else {
		return nil, fmt.Errorf("unsupported archive type: %s", sourceArchive)
	}

	tarReader := tar.NewReader(archiveReader)
	buf := bytes.NewBuffer(nil)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if hdr.Name != pathInArchive {
			continue
		}
		if _, err := io.Copy(buf, tarReader); err != nil {
			return nil, err
		}
		break
	}
	if buf.Len() == 0 {
		return nil, fmt.Errorf("module file not found in archive %s under path %s", sourceArchive, pathInArchive)
	}
	return buf.Bytes(), nil
}

func sourceArchiveIntegrity(sourceArchive string) (string, error) {
	archive, err := os.Open(sourceArchive)
	if err != nil {
		return "", err
	}
	defer archive.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, archive); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256-%s", base64.StdEncoding.EncodeToString((hash.Sum(nil)))), nil
}

func copySourceArchive(sourceArchive, destination string) error {
	if err := os.Link(sourceArchive, destination); err == nil {
		return nil
	}
	inFile, err := os.Open(sourceArchive)
	if err != nil {
		return err
	}
	defer inFile.Close()
	outFile, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, err = io.Copy(outFile, inFile)
	return err
}

func extractSourceArchive(sourceArchive, destination string, stripPrefix string) error {
	archive, err := os.Open(sourceArchive)
	if err != nil {
		return err
	}
	defer archive.Close()

	var archiveReader io.ReadCloser
	if filepath.Ext(sourceArchive) == ".tar" {
		archiveReader = archive
	} else if filepath.Ext(sourceArchive) == ".gz" {
		archiveReader, err = gzip.NewReader(archive)
		if err != nil {
			return err
		}
		defer archiveReader.Close()
	} else {
		return fmt.Errorf("unsupported archive type: %s", sourceArchive)
	}

	if err := os.Mkdir(destination, os.ModePerm); err != nil {
		return err
	}

	tarReader := tar.NewReader(archiveReader)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		} else if err != nil {
			return fmt.Errorf("failed to read tarball: %v", err)
		}
		if len(hdr.Name) == 0 || hdr.Name[0] == '.' || hdr.Name[0] == '/' {
			return fmt.Errorf("invalid file path in archive: %s", hdr.Name)
		}
		if stripPrefix != "" {
			if !strings.HasPrefix(hdr.Name, stripPrefix) {
				continue
			}
			hdr.Name = hdr.Name[len(stripPrefix)+1:]
		}
		fInfo := hdr.FileInfo()
		if fInfo.IsDir() {
			os.Mkdir(filepath.Join(destination, hdr.Name), fInfo.Mode())
			continue
		}
		if !fInfo.Mode().IsRegular() {
			return fmt.Errorf("unsupported file type: %s", hdr.Name)
		}
		file, err := os.Create(filepath.Join(destination, hdr.Name))
		if err != nil {
			return fmt.Errorf("failed to create file: %v", err)
		}
		_, err = io.Copy(file, tarReader)
		file.Close()
		if err != nil {
			return fmt.Errorf("failed to extract file: %v", err)
		}
	}
	return nil
}

const sourcesBasePath = "contents"
