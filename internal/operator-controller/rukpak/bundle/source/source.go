package source

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing/fstest"

	"helm.sh/helm/v3/pkg/chart"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"sigs.k8s.io/yaml"

	"github.com/operator-framework/api/pkg/operators/v1alpha1"
	"github.com/operator-framework/operator-registry/alpha/property"

	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/bundle"
	registry "github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/operator-registry"
)

const (
	PropertyOLMProperties = "olm.properties"
)

// ContentType identifies the packaging format of a pulled direct-source
// artifact. Direct sources are content-sniffed: the same sourceType can
// carry different packaging formats.
type ContentType string

const (
	// ContentRegistryV1 is a registry+v1 operator bundle
	// (manifests/ + metadata/annotations.yaml layout).
	ContentRegistryV1 ContentType = "registry+v1"
	// ContentHelm is a standard Helm chart (Chart.yaml at the root),
	// e.g. an artifact pushed with `helm push` to an OCI registry.
	ContentHelm ContentType = "helm"
)

// SniffContentType inspects the layout of a pulled artifact filesystem and
// reports its packaging format. Helm content is recognized in three
// shapes: a root Chart.yaml (extracted chart), a single root *.tgz (the
// layer of a standard chart OCI artifact, before unpacking), and a single
// root directory containing Chart.yaml (the same artifact after its
// tar+gzip layer has been unpacked by the image cache). The registry+v1
// layout is otherwise required.
func SniffContentType(fsys fs.FS) (ContentType, error) {
	if _, err := fs.Stat(fsys, "Chart.yaml"); err == nil {
		return ContentHelm, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if isStandardChartArtifact(fsys) {
		return ContentHelm, nil
	}
	if isUnpackedChartArtifact(fsys) {
		return ContentHelm, nil
	}
	if _, err := fs.Stat(fsys, filepath.Join("metadata", "annotations.yaml")); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("unrecognized content: neither a Helm chart (Chart.yaml), a chart OCI artifact (*.tgz or its unpacked chart directory), nor a registry+v1 bundle (metadata/annotations.yaml)")
		}
		return "", err
	}
	return ContentRegistryV1, nil
}

// isStandardChartArtifact reports whether the filesystem holds exactly one
// top-level .tgz entry, the chart layer of a `helm push` artifact.
func isStandardChartArtifact(fsys fs.FS) bool {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return false
	}
	if len(entries) != 1 || entries[0].IsDir() {
		return false
	}
	return strings.HasSuffix(entries[0].Name(), ".tgz")
}

// isUnpackedChartArtifact reports whether the filesystem holds a single
// top-level directory containing a Chart.yaml: the shape a chart
// OCI artifact's tar+gzip layer leaves after the image cache unpacks it.
func isUnpackedChartArtifact(fsys fs.FS) bool {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return false
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		return false
	}
	_, err = fs.Stat(fsys, filepath.Join(entries[0].Name(), "Chart.yaml"))
	return err == nil
}

// LoadChartMetadata reads the root Chart.yaml of a Helm chart source.
func LoadChartMetadata(fsys fs.FS) (*chart.Metadata, error) {
	data, err := fs.ReadFile(fsys, "Chart.yaml")
	if err != nil {
		return nil, fmt.Errorf("read Chart.yaml: %w", err)
	}
	meta := &chart.Metadata{}
	if err := yaml.Unmarshal(data, meta); err != nil {
		return nil, fmt.Errorf("parse Chart.yaml: %w", err)
	}
	if meta.Name == "" || meta.Version == "" {
		return nil, fmt.Errorf("Chart.yaml must set both name and version")
	}
	return meta, nil
}

// ReadChartFS returns the effective chart filesystem for either Helm
// content layout: an extracted chart (root Chart.yaml) is returned as-is;
// a standard chart OCI artifact (a single root *.tgz, the layout produced
// by `helm push`) is extracted with its leading chart-name directory
// stripped so Chart.yaml sits at the root.
func ReadChartFS(fsys fs.FS) (fs.FS, error) {
	if _, err := fs.Stat(fsys, "Chart.yaml"); err == nil {
		return fsys, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		if _, err := fs.Stat(fsys, filepath.Join(entries[0].Name(), "Chart.yaml")); err == nil {
			return fs.Sub(fsys, entries[0].Name())
		}
		return nil, fmt.Errorf("not a Helm chart: single root directory has no Chart.yaml")
	}
	if len(entries) != 1 || entries[0].IsDir() || !strings.HasSuffix(entries[0].Name(), ".tgz") {
		return nil, fmt.Errorf("not a Helm chart: no root Chart.yaml and no single *.tgz archive")
	}
	archiveData, err := fs.ReadFile(fsys, entries[0].Name())
	if err != nil {
		return nil, err
	}
	out := fstest.MapFS{}
	tr := tar.NewReader(bytes.NewReader(archiveData))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read chart archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Strip the leading "<chart-name>/" directory.
		name := strings.TrimPrefix(hdr.Name, "./")
		if idx := strings.IndexByte(name, '/'); idx >= 0 {
			name = name[idx+1:]
		} else {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read %q from chart archive: %w", hdr.Name, err)
		}
		out[name] = &fstest.MapFile{Data: data}
	}
	if _, err := fs.Stat(out, "Chart.yaml"); err != nil {
		return nil, fmt.Errorf("chart archive does not contain a Chart.yaml at its root")
	}
	return out, nil
}

type BundleSource interface {
	GetBundle() (bundle.RegistryV1, error)
}

type RegistryV1Properties struct {
	Properties []property.Property `json:"properties"`
}

// identitySource is a bundle source that returns itself
type identitySource bundle.RegistryV1

func (r identitySource) GetBundle() (bundle.RegistryV1, error) {
	return bundle.RegistryV1(r), nil
}

func FromBundle(rv1 bundle.RegistryV1) BundleSource {
	return identitySource(rv1)
}

// FromFS returns a BundleSource that loads a registry+v1 bundle from a filesystem.
// The filesystem is expected to conform to the registry+v1 format:
// metadata/annotations.yaml
// metadata/properties.yaml
// manifests/
//   - csv.yaml
//   - ...
//
// manifests directory should not contain subdirectories
func FromFS(fs fs.FS) BundleSource {
	return fsBundleSource{
		FS: fs,
	}
}

type fsBundleSource struct {
	FS fs.FS
}

func (f fsBundleSource) GetBundle() (bundle.RegistryV1, error) {
	reg := bundle.RegistryV1{}
	annotationsFileData, err := fs.ReadFile(f.FS, filepath.Join("metadata", "annotations.yaml"))
	if err != nil {
		return reg, err
	}
	annotationsFile := registry.AnnotationsFile{}
	if err := yaml.Unmarshal(annotationsFileData, &annotationsFile); err != nil {
		return reg, err
	}
	reg.PackageName = annotationsFile.Annotations.PackageName

	const manifestsDir = "manifests"
	foundCSV := false
	if err := fs.WalkDir(f.FS, manifestsDir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			if path == manifestsDir {
				return nil
			}
			return fmt.Errorf("subdirectories are not allowed within the %q directory of the bundle image filesystem: found %q", manifestsDir, path)
		}
		manifestFile, err := f.FS.Open(path)
		if err != nil {
			return err
		}
		defer manifestFile.Close()

		result := resource.NewLocalBuilder().Unstructured().Flatten().Stream(manifestFile, path).Do()
		if err := result.Err(); err != nil {
			return err
		}
		if err := result.Visit(func(info *resource.Info, err error) error {
			if err != nil {
				return err
			}
			switch info.Object.GetObjectKind().GroupVersionKind().Kind {
			case "ClusterServiceVersion":
				csv := v1alpha1.ClusterServiceVersion{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(info.Object.(*unstructured.Unstructured).Object, &csv); err != nil {
					return err
				}
				reg.CSV = csv
				foundCSV = true
			case "CustomResourceDefinition":
				crd := apiextensionsv1.CustomResourceDefinition{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(info.Object.(*unstructured.Unstructured).Object, &crd); err != nil {
					return err
				}
				reg.CRDs = append(reg.CRDs, crd)
			default:
				reg.Others = append(reg.Others, *info.Object.(*unstructured.Unstructured))
			}
			return nil
		}); err != nil {
			return fmt.Errorf("error parsing objects in %q: %v", path, err)
		}
		return nil
	}); err != nil {
		return reg, err
	}

	if !foundCSV {
		return reg, fmt.Errorf("no ClusterServiceVersion found in %q", manifestsDir)
	}

	if err := copyMetadataPropertiesToCSV(&reg.CSV, f.FS); err != nil {
		return reg, err
	}

	return reg, nil
}

// copyMetadataPropertiesToCSV copies properties from `metadata/propeties.yaml` (in the filesystem fsys) into
// the CSV's `.metadata.annotations['olm.properties']` value, preserving any properties that are already
// present in the annotations.
func copyMetadataPropertiesToCSV(csv *v1alpha1.ClusterServiceVersion, fsys fs.FS) error {
	var allProperties []property.Property

	// First load existing properties from the CSV. We want to preserve these.
	if csvPropertiesJSON, ok := csv.Annotations["olm.properties"]; ok {
		var csvProperties []property.Property
		if err := json.Unmarshal([]byte(csvPropertiesJSON), &csvProperties); err != nil {
			return fmt.Errorf("failed to unmarshal csv.metadata.annotations['olm.properties']: %w", err)
		}
		allProperties = append(allProperties, csvProperties...)
	}

	// Next, load properties from the metadata/properties.yaml file, if it exists.
	metadataPropertiesJSON, err := fs.ReadFile(fsys, filepath.Join("metadata", "properties.yaml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("failed to read properties.yaml file: %w", err)
	}

	// If there are no properties, we can stick with whatever
	// was already present in the CSV annotations.
	if len(metadataPropertiesJSON) == 0 {
		return nil
	}

	// Otherwise, we need to parse the properties.yaml file and
	// append its properties into the CSV annotation.
	var metadataProperties RegistryV1Properties
	if err := yaml.Unmarshal(metadataPropertiesJSON, &metadataProperties); err != nil {
		return fmt.Errorf("failed to unmarshal metadata/properties.yaml: %w", err)
	}
	allProperties = append(allProperties, metadataProperties.Properties...)

	// Lastly re-marshal all the properties back into a JSON array and update the CSV annotation
	allPropertiesJSON, err := json.Marshal(allProperties)
	if err != nil {
		return fmt.Errorf("failed to marshal registry+v1 properties to json: %w", err)
	}
	if csv.Annotations == nil {
		csv.Annotations = map[string]string{}
	}
	csv.Annotations[PropertyOLMProperties] = string(allPropertiesJSON)
	return nil
}
