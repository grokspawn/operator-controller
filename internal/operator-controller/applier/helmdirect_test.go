package applier

import (
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/operator-controller/rukpak/bundle/source"
)

func helmChartMapFS(template string, extra ...map[string]string) fstest.MapFS {
	out := fstest.MapFS{
		"Chart.yaml": &fstest.MapFile{Data: []byte(
			"apiVersion: v2\nname: kubecon-chart-demo\nversion: 0.3.0\n")},
		"values.yaml": &fstest.MapFile{Data: []byte(
			"replicaCount: 1\n")},
		"templates/configmap.yaml": &fstest.MapFile{Data: []byte(template)},
	}
	for _, m := range extra {
		for k, v := range m {
			out[k] = &fstest.MapFile{Data: []byte(v)}
		}
	}
	return out
}

func helmTestExtension() *ocv1.ClusterExtension {
	return &ocv1.ClusterExtension{
		ObjectMeta: metav1.ObjectMeta{Name: "kubecon-demo"},
		Spec: ocv1.ClusterExtensionSpec{
			Namespace: "kubecon-demo-system",
			Source: ocv1.SourceConfig{
				SourceType: ocv1.SourceTypeOCIImage,
				OCIImage:   ocv1.OCIImageSource{Ref: "registry.example/chart:v0.3.0"},
			},
		},
	}
}

func inlineConfig(raw string) *ocv1.ClusterExtensionConfig {
	return &ocv1.ClusterExtensionConfig{
		ConfigType: "Inline",
		Inline:     &apiextensionsv1.JSON{Raw: []byte(raw)},
	}
}

func TestHelmManifestProviderRendersChart(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data:
  replicas: "{{ .Values.replicaCount }}"
  releaseName: "{{ .Release.Name }}"
  isUpgrade: "{{ .Release.IsUpgrade }}"
`)
	ext := helmTestExtension()
	ext.Spec.Config = inlineConfig(`{"replicaCount": 3}`)

	provider := &HelmManifestProvider{}
	objs, err := provider.Get(chartFS, ext)
	require.NoError(t, err)
	require.Len(t, objs, 1)

	cm := objs[0].(*unstructured.Unstructured)
	require.Equal(t, "demo-cm", objs[0].GetName())
	require.Equal(t, "3", cm.Object["data"].(map[string]interface{})["replicas"])
	require.Equal(t, "kubecon-demo", cm.Object["data"].(map[string]interface{})["releaseName"])
	require.Equal(t, "true", cm.Object["data"].(map[string]interface{})["isUpgrade"])
}

func TestHelmManifestProviderValidatesValuesSchema(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data:
  replicas: "{{ .Values.replicaCount }}"
`, map[string]string{
		"values.schema.json": `{
  "$schema": "https://json-schema.org/draft-07/schema#",
  "type": "object",
  "properties": {"replicaCount": {"type": "integer", "minimum": 1, "maximum": 5}},
  "required": ["replicaCount"]
}`,
	})

	ext := helmTestExtension()
	ext.Spec.Config = inlineConfig(`{"replicaCount": 99}`)
	_, err := (&HelmManifestProvider{}).Get(chartFS, ext)
	require.ErrorContains(t, err, "schema validation")

	ext.Spec.Config = inlineConfig(`{"replicaCount": 2}`)
	objs, err := (&HelmManifestProvider{}).Get(chartFS, ext)
	require.NoError(t, err)
	require.Len(t, objs, 1)
}

func TestHelmManifestProviderRejectsBannedTemplateFunction(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: Secret
metadata:
  name: demo-secret
type: Opaque
stringData:
  password: {{ randAlphaNum 20 }}
`)
	_, err := (&HelmManifestProvider{}).Get(chartFS, helmTestExtension())
	require.ErrorContains(t, err, "randAlphaNum")
	require.ErrorContains(t, err, "deterministic rendering subset")
}

func TestHelmManifestProviderRejectsHookAnnotation(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
  annotations:
    helm.sh/hook: pre-install
data: {}
`)
	_, err := (&HelmManifestProvider{}).Get(chartFS, helmTestExtension())
	require.ErrorContains(t, err, "helm.sh/hook")
}

func TestHelmManifestProviderRejectsKeepResourcePolicy(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
  annotations:
    helm.sh/resource-policy: keep
data: {}
`)
	_, err := (&HelmManifestProvider{}).Get(chartFS, helmTestExtension())
	require.ErrorContains(t, err, "helm.sh/resource-policy")
}

func TestHelmManifestProviderRejectsRemoteDependency(t *testing.T) {
	chartFS := fstest.MapFS{
		"Chart.yaml": &fstest.MapFile{Data: []byte(
			"apiVersion: v2\nname: kubecon-chart-demo\nversion: 0.3.0\ndependencies:\n  - name: subchart\n    repository: https://charts.example.com\n    version: 1.0.0\n")},
	}
	_, err := (&HelmManifestProvider{}).Get(chartFS, helmTestExtension())
	require.ErrorContains(t, err, "vendored")
}

func TestHelmManifestProviderRequiresNonEmptyInlineValues(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data: {}
`)
	ext := helmTestExtension()
	ext.Spec.Config = inlineConfig(`{}`)
	_, err := (&HelmManifestProvider{}).Get(chartFS, ext)
	require.ErrorContains(t, err, "non-empty values object")
}

func TestDispatchingManifestProviderRoutesByContent(t *testing.T) {
	chartFS := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data: {}
`)
	dispatcher := NewDispatchingManifestProvider(
		&RegistryV1ManifestProvider{},
		&HelmManifestProvider{},
	)

	objs, err := dispatcher.Get(chartFS, helmTestExtension())
	require.NoError(t, err)
	require.Len(t, objs, 1)
	require.Equal(t, "ConfigMap", objs[0].GetObjectKind().GroupVersionKind().Kind)
}

func TestSniffContentType(t *testing.T) {
	helm := helmChartMapFS(`apiVersion: v1
kind: ConfigMap
metadata:
  name: demo-cm
data: {}
`)
	contentType, err := source.SniffContentType(helm)
	require.NoError(t, err)
	require.Equal(t, source.ContentHelm, contentType)

	var regv1 fs.FS = fstest.MapFS{
		"metadata/annotations.yaml": &fstest.MapFile{Data: []byte("annotations: {}\n")},
	}
	contentType, err = source.SniffContentType(regv1)
	require.NoError(t, err)
	require.Equal(t, source.ContentRegistryV1, contentType)

	unknown := fstest.MapFS{"README.md": &fstest.MapFile{Data: []byte("hi")}}
	_, err = source.SniffContentType(unknown)
	require.ErrorContains(t, err, "unrecognized content")
}
