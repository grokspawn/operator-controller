package resolve

import (
	"context"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
)

// helmChartFS builds a minimal standard Helm chart filesystem: Chart.yaml
// at the root plus a template.
func helmChartFS(t *testing.T, template string) fs.FS {
	t.Helper()
	return fstest.MapFS{
		"Chart.yaml": &fstest.MapFile{Data: []byte("apiVersion: v2\nname: kubecon-chart-demo\nversion: 0.3.0\n")},
		"values.yaml": &fstest.MapFile{Data: []byte(
			"replicaCount: 1\n")},
		"templates/deployment.yaml": &fstest.MapFile{Data: []byte(template)},
	}
}

func TestOCIImageResolverResolvesHelmChart(t *testing.T) {
	ref := "quay.io/example/chart@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bundleFS := helmChartFS(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  replicas: {{ .Values.replicaCount }}
`)

	resolver := &OCIImageResolver{Puller: fakePuller{fs: bundleFS, ref: ref}, Cache: fakeCache{}}
	ext := &ocv1.ClusterExtension{Spec: ocv1.ClusterExtensionSpec{Source: ocv1.SourceConfig{
		SourceType: ocv1.SourceTypeOCIImage,
		OCIImage:   ocv1.OCIImageSource{Ref: ref},
	}}}

	bundle, version, deprecation, err := resolver.Resolve(context.Background(), ext, nil)
	require.NoError(t, err)
	require.Equal(t, "kubecon-chart-demo.v0.3.0", bundle.Name)
	require.Equal(t, "kubecon-chart-demo", bundle.Package)
	require.Equal(t, ref, bundle.Image)
	require.Equal(t, "0.3.0", version.Version.String())
	require.Len(t, bundle.Properties, 1)
	require.Equal(t, "olm.package", bundle.Properties[0].Type)
	require.Nil(t, deprecation)
}

func TestOCIImageResolverRejectsChartWithoutVersion(t *testing.T) {
	ref := "quay.io/example/chart@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	bundleFS := fstest.MapFS{
		"Chart.yaml": &fstest.MapFile{Data: []byte("apiVersion: v2\nname: incomplete\n")},
	}

	resolver := &OCIImageResolver{Puller: fakePuller{fs: bundleFS, ref: ref}, Cache: fakeCache{}}
	ext := &ocv1.ClusterExtension{Spec: ocv1.ClusterExtensionSpec{Source: ocv1.SourceConfig{
		SourceType: ocv1.SourceTypeOCIImage,
		OCIImage:   ocv1.OCIImageSource{Ref: ref},
	}}}

	_, _, _, err := resolver.Resolve(context.Background(), ext, nil)
	require.ErrorContains(t, err, "Chart.yaml must set both name and version")
}

func TestOCIImageResolverRejectsUnrecognizedContent(t *testing.T) {
	ref := "quay.io/example/mystery@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	bundleFS := fstest.MapFS{
		"README.md": &fstest.MapFile{Data: []byte("not a bundle")},
	}

	resolver := &OCIImageResolver{Puller: fakePuller{fs: bundleFS, ref: ref}, Cache: fakeCache{}}
	ext := &ocv1.ClusterExtension{Spec: ocv1.ClusterExtensionSpec{Source: ocv1.SourceConfig{
		SourceType: ocv1.SourceTypeOCIImage,
		OCIImage:   ocv1.OCIImageSource{Ref: ref},
	}}}

	_, _, _, err := resolver.Resolve(context.Background(), ext, nil)
	require.ErrorContains(t, err, "unrecognized content")
}
