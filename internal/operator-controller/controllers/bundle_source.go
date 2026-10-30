/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"reflect"

	"github.com/operator-framework/operator-registry/alpha/declcfg"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
)

// BundleSource owns resolution and lifecycle policies for an installation source.
// Resolve includes rollout continuation and recovery from resolution errors.
// SetDeprecationStatus is called even on resolution errors, and again after a
// successful apply with the newly installed bundle name.
type BundleSource interface {
	Resolve(context.Context, *ocv1.ClusterExtension, *RevisionStates) (BundleResolution, error)
	SetDeprecationStatus(ext *ocv1.ClusterExtension, installedBundleName string, resolution BundleResolution)
	ReconcileOnCatalogChange(*ocv1.ClusterExtension, *ocv1.ClusterCatalog) bool
	CanMaintainWithoutContent(*ocv1.ClusterExtension, *RevisionStates, BundleResolution) bool
}

// BundleResolution carries the selected revision and source-provided deprecation
// evidence. A nil Deprecation with DeprecationKnown true means known-empty data.
// Results returned alongside an error still carry evidence for status reporting.
type BundleResolution struct {
	Revision         *RevisionMetadata
	Deprecation      *declcfg.Deprecation
	DeprecationKnown bool
}

// BundleSources selects a source policy using spec.source.sourceType.
type BundleSources map[string]BundleSource

// For returns the configured policy or an error before any source is invoked.
func (sources BundleSources) For(ext *ocv1.ClusterExtension) (BundleSource, error) {
	source := sources[ext.Spec.Source.SourceType]
	if source == nil || nilBundleSource(source) {
		return nil, fmt.Errorf("unsupported sourceType %q: bundle source is not configured", ext.Spec.Source.SourceType)
	}
	return source, nil
}

// Interfaces can contain typed nil implementations; reject these at lookup too.
func nilBundleSource(source BundleSource) bool {
	v := reflect.ValueOf(source)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
