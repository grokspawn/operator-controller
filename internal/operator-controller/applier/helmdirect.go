package applier

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/bundle/source"
	registrybundle "github.com/operator-framework/operator-registry/pkg/lib/bundle"
)

// bannedTemplateFunctions are Helm template functions whose output changes
// between renders for identical inputs. Continuous reconciliation needs
// reproducible output, so charts using them are rejected with an error
// instead of silently producing unstable resources.
var bannedTemplateFunctions = []string{
	"randAlphaNum", "randAlpha", "randAscii", "randNumeric",
	"uuidv4", "now", "date", "dateInZone", "dateModify", "toDate",
	"unixEpoch", "duration", "durationRound", "htmlDate", "htmlDateInZone",
	"env", "expandenv", "getHostByName",
}

// bannedAnnotations are chart-authored annotations that would conflict
// with extension ownership: hooks escape OLM's phased lifecycle, and keep
// policies would prevent cleanup of managed content.
var bannedAnnotations = []string{
	"helm.sh/hook",
	"helm.sh/hook-delete-policy",
	"helm.sh/resource-policy",
}

// HelmManifestProvider renders a standard Helm chart into the objects OLM
// manages for a ClusterExtension. It implements the restricted
// deterministic rendering subset of the helm+v2 content proposal:
//
//   - values come from spec.config.inline (merged over chart defaults,
//     validated against values.schema.json when the chart provides one)
//   - hooks and resource-policy: keep are rejected
//   - templates using non-deterministic functions are rejected
//   - .Release is mapped onto the extension identity, with proposed
//     always-upgrade semantics (IsUpgrade=true, IsInstall=false,
//     Revision fixed at 1 for stable reconciliation)
//
// Rendered objects then flow through OLM's existing application,
// phased-rollout, readiness and ownership machinery. This is not Helm
// release management exposed through another API.
type HelmManifestProvider struct{}

func (h *HelmManifestProvider) Get(chartFS fs.FS, ext *ocv1.ClusterExtension) ([]client.Object, error) {
	chartLoaded, err := loadChartFromFS(chartFS)
	if err != nil {
		return nil, err
	}

	if err := validateChart(chartLoaded); err != nil {
		return nil, err
	}

	values, err := h.valuesFor(ext)
	if err != nil {
		return nil, err
	}

	if err := chartutil.ValidateAgainstSchema(chartLoaded, values); err != nil {
		return nil, fmt.Errorf("chart values failed schema validation: %w", err)
	}

	rendered, err := renderChart(chartLoaded, values, ext)
	if err != nil {
		return nil, err
	}

	objs, err := decodeRenderedManifests(rendered)
	if err != nil {
		return nil, err
	}

	return stampNamespaces(objs, ext), nil
}

// stampNamespaces sets the install namespace on namespace-scoped rendered
// objects, mirroring the registry+v1 render path. Kinds in the shared
// supported-resources table use its scope; unknown kinds (typically
// custom resources owned by charts) are treated as namespaced, which is
// the convention for chart-rendered content.
func stampNamespaces(objs []client.Object, ext *ocv1.ClusterExtension) []client.Object {
	for _, obj := range objs {
		if obj.GetNamespace() != "" {
			continue
		}
		namespaced := true
		if _, known := registrybundle.IsSupported(obj.GetObjectKind().GroupVersionKind().Kind); known {
			_, isNamespaced := registrybundle.IsSupported(obj.GetObjectKind().GroupVersionKind().Kind)
			namespaced = bool(isNamespaced)
		}
		if namespaced {
			obj.SetNamespace(ext.Spec.Namespace)
		}
	}
	return objs
}

// valuesFor extracts user-supplied values from the ClusterExtension's
// inline configuration. For Helm content the inline object is the values
// override, following the same config mechanism as deploymentConfig.
func (h *HelmManifestProvider) valuesFor(ext *ocv1.ClusterExtension) (map[string]interface{}, error) {
	if ext.Spec.Config == nil || ext.Spec.Config.ConfigType != "Inline" || ext.Spec.Config.Inline == nil {
		return map[string]interface{}{}, nil
	}
	values := map[string]interface{}{}
	if err := yaml.Unmarshal(ext.Spec.Config.Inline.Raw, &values); err != nil {
		return nil, fmt.Errorf("parse inline values: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("inline configuration for Helm content must be a non-empty values object")
	}
	return values, nil
}

// loadChartFromFS adapts an fs.FS (a pulled OCI artifact) into Helm's
// chart loader, handling both the extracted-chart and standard tgz
// artifact layouts.
func loadChartFromFS(chartFS fs.FS) (*chart.Chart, error) {
	effectiveFS, err := source.ReadChartFS(chartFS)
	if err != nil {
		return nil, err
	}
	var files []*loader.BufferedFile
	err = fs.WalkDir(effectiveFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(effectiveFS, path)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path, "./")
		files = append(files, &loader.BufferedFile{Name: name, Data: data})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read chart files: %w", err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	chartLoaded, err := loader.LoadFiles(files)
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}
	return chartLoaded, nil
}

// validateChart enforces the supported subset on chart structure and
// template sources before rendering.
func validateChart(c *chart.Chart) error {
	if c.Metadata == nil {
		return fmt.Errorf("chart has no metadata (Chart.yaml)")
	}
	if err := validateTemplateSources(c); err != nil {
		return err
	}
	// Dependencies must be vendored: runtime fetching is unsupported and
	// charts/ contents are already part of the artifact.
	for _, dep := range c.Metadata.Dependencies {
		if dep.Repository != "" && !strings.HasPrefix(dep.Repository, "file://") {
			return fmt.Errorf("chart dependency %q uses a remote repository %q; dependencies must be vendored into charts/", dep.Name, dep.Repository)
		}
	}
	return nil
}

// validateTemplateSources rejects templates that use banned functions.
func validateTemplateSources(c *chart.Chart) error {
	for _, tpl := range c.Templates {
		for _, fn := range bannedTemplateFunctions {
			// Match the function invocation, not substring occurrences in
			// unrelated identifiers.
			if bytes.Contains(tpl.Data, []byte(fn+" ")) || bytes.Contains(tpl.Data, []byte(fn+"}}")) || bytes.Contains(tpl.Data, []byte(fn+")")) {
				return fmt.Errorf(
					"template %q uses %q, which is not part of the supported deterministic rendering subset; produce stable output for stable inputs instead",
					tpl.Name, fn)
			}
		}
	}
	return nil
}

// renderChart renders with extension-mapped release options and then
// enforces the output-side restrictions (banned annotations).
func renderChart(c *chart.Chart, values map[string]interface{}, ext *ocv1.ClusterExtension) (map[string]string, error) {
	options := chartutil.ReleaseOptions{
		Name:      ext.Name,
		Namespace: ext.Spec.Namespace,
		// Proposed always-upgrade semantics from the helm+v2 content
		// design: renders behave like upgrades, and the revision is fixed
		// so identical inputs produce identical output.
		IsUpgrade: true,
		IsInstall: false,
		Revision:  1,
	}
	caps := chartutil.DefaultCapabilities
	toRender, err := chartutil.ToRenderValues(c, values, options, caps)
	if err != nil {
		return nil, fmt.Errorf("prepare render values: %w", err)
	}
	rendered, err := engine.Render(c, toRender)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}
	for name, manifest := range rendered {
		if err := rejectBannedAnnotations(name, manifest); err != nil {
			return nil, err
		}
	}
	return rendered, nil
}

func rejectBannedAnnotations(name, manifest string) error {
	for _, annotation := range bannedAnnotations {
		if strings.Contains(manifest, annotation+":") {
			return fmt.Errorf(
				"rendered manifest %q sets %q, which is unsupported: hooks and keep policies conflict with extension-managed lifecycle and cleanup",
				name, annotation)
		}
	}
	return nil
}

// decodeRenderedManifests parses rendered YAML documents into client
// objects, skipping empty documents.
func decodeRenderedManifests(rendered map[string]string) ([]client.Object, error) {
	objs := []client.Object{}
	names := make([]string, 0, len(rendered))
	for name := range rendered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		manifest := rendered[name]
		decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(manifest), 4096)
		for {
			u := &unstructured.Unstructured{}
			if err := decoder.Decode(u); err != nil {
				break
			}
			if u.Object == nil {
				continue
			}
			objs = append(objs, u)
		}
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("chart rendered no Kubernetes objects")
	}
	return objs, nil
}

// dispatchingManifestProvider routes rendering by the packaging format of
// the pulled artifact: registry+v1 bundles use the existing provider;
// Helm charts use the restricted Helm renderer. Both then flow through
// the same ClusterObjectSet lifecycle.
type dispatchingManifestProvider struct {
	RegistryV1 ManifestProvider
	Helm       ManifestProvider
}

func NewDispatchingManifestProvider(registryV1, helm ManifestProvider) ManifestProvider {
	return &dispatchingManifestProvider{RegistryV1: registryV1, Helm: helm}
}

func (d *dispatchingManifestProvider) Get(bundleFS fs.FS, ext *ocv1.ClusterExtension) ([]client.Object, error) {
	contentType, err := source.SniffContentType(bundleFS)
	if err != nil {
		return nil, err
	}
	switch contentType {
	case source.ContentHelm:
		if d.Helm == nil {
			return nil, fmt.Errorf("Helm chart content is not configured on this controller")
		}
		return d.Helm.Get(bundleFS, ext)
	case source.ContentRegistryV1:
		return d.RegistryV1.Get(bundleFS, ext)
	}
	return nil, fmt.Errorf("unsupported content type %q", contentType)
}

// ensure interface conformance
var _ ManifestProvider = (*HelmManifestProvider)(nil)
