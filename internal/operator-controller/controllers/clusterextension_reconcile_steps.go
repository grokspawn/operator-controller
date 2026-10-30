/*
Copyright 2025.

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
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/finalizer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ocv1 "github.com/operator-framework/operator-controller/api/v1"
	"github.com/operator-framework/operator-controller/internal/shared/labels"
	imageutil "github.com/operator-framework/operator-controller/internal/shared/util/image"
)

func HandleFinalizers(f finalizer.Finalizer) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		l := log.FromContext(ctx)

		l.Info("handling finalizers")
		finalizeResult, err := f.Finalize(ctx, ext)
		if err != nil {
			setStatusProgressing(ext, err)
			return nil, err
		}
		if finalizeResult.Updated || finalizeResult.StatusUpdated {
			// On create: make sure the finalizer is applied before we do anything
			// On delete: make sure we do nothing after the finalizer is removed
			return &ctrl.Result{}, nil
		}

		if ext.GetDeletionTimestamp() != nil {
			// If we've gotten here, that means the cluster extension is being deleted, we've handled all of
			// _our_ finalizers (above), but the cluster extension is still present in the cluster, likely
			// because there are _other_ finalizers that other controllers need to handle, (e.g. the orphan
			// deletion finalizer).
			return &ctrl.Result{}, nil
		}
		return nil, nil
	}
}

// ClusterExtensionValidator is a function that validates a ClusterExtension.
// It returns an error if validation fails. Validators are executed sequentially
// in the order they are registered.
type ClusterExtensionValidator func(context.Context, *ocv1.ClusterExtension) error

// ValidateClusterExtension returns a ReconcileStepFunc that executes all
// validators sequentially. All validators are executed even if some fail,
// and all errors are collected and returned as a joined error.
// This provides complete validation feedback in a single reconciliation cycle.
func ValidateClusterExtension(validators ...ClusterExtensionValidator) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		l := log.FromContext(ctx)

		l.V(1).Info("validating cluster extension")
		var validationErrors []error
		for _, validator := range validators {
			if err := validator(ctx, ext); err != nil {
				validationErrors = append(validationErrors, err)
			}
		}

		// If there are no validation errors, continue reconciliation
		if len(validationErrors) == 0 {
			return nil, nil
		}

		// Set status conditions with the validation errors
		err := fmt.Errorf("operation cannot proceed due to the following validation error(s): %w", errors.Join(validationErrors...))
		setInstalledStatusConditionUnknown(ext, err.Error())
		setStatusProgressing(ext, err)
		return nil, err
	}
}

// ServiceAccountDeprecationWarning returns a validator that checks if the deprecated
// serviceAccount field is populated and logs a warning if found.
func ServiceAccountDeprecationWarning() ClusterExtensionValidator {
	return func(ctx context.Context, ext *ocv1.ClusterExtension) error {
		l := log.FromContext(ctx)
		if len(ext.Spec.ServiceAccount.Name) > 0 { //nolint:staticcheck // intentional read of deprecated field to emit deprecation warning
			l.Info("spec.serviceAccount is deprecated, ignored, and will be removed in a future release - operator-controller's cluster-admin service account is used for all cluster interactions")
		}
		return nil
	}
}

func RetrieveRevisionStates(r RevisionStatesGetter) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		l := log.FromContext(ctx)
		l.Info("getting installed bundle")
		revisionStates, err := r.GetRevisionStates(ctx, ext)
		if err != nil {
			setInstallStatus(ext, nil)
			setInstalledStatusConditionUnknown(ext, err.Error())
			setStatusProgressing(ext, errors.New("retrying to get installed bundle"))
			return nil, err
		}
		state.revisionStates = revisionStates
		return nil, nil
	}
}

// ResolveBundle selects the extension's source policy before resolving, including
// when a revision is already rolling out. Sources own recovery and status evidence.
func ResolveBundle(sources BundleSources) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		source, err := sources.For(ext)
		if err != nil {
			setStatusProgressing(ext, err)
			setInstalledStatusFromRevisionStates(ext, state.revisionStates)
			ensureFailureConditionsWithReason(ext, ocv1.ReasonRetrying, fmt.Sprintf("failed to resolve bundle: %v", err))
			return nil, err
		}
		state.bundleSource = source
		resolution, err := source.Resolve(ctx, ext, state.revisionStates)
		state.bundleResolution = resolution
		installedBundleName := ""
		if state.revisionStates.Installed != nil {
			installedBundleName = state.revisionStates.Installed.Name
		}

		source.SetDeprecationStatus(ext, installedBundleName, resolution)

		if err != nil {
			setStatusProgressing(ext, err)
			setInstalledStatusFromRevisionStates(ext, state.revisionStates)
			ensureFailureConditionsWithReason(ext, ocv1.ReasonRetrying, fmt.Sprintf("failed to resolve bundle: %v", err))
			return nil, err
		}
		state.resolvedRevisionMetadata = resolution.Revision
		return nil, nil
	}
}

func UnpackBundle(i imageutil.Puller, cache imageutil.Cache) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		l := log.FromContext(ctx)

		// Defensive check: resolvedRevisionMetadata should be set by ResolveBundle step
		if state.resolvedRevisionMetadata == nil {
			return nil, fmt.Errorf("unable to retrieve bundle information")
		}

		// Always try to pull the bundle content (Pull uses cache-first strategy, so this is efficient)
		l.V(1).Info("pulling bundle content")
		imageFS, _, _, err := i.Pull(ctx, ext.GetName(), state.resolvedRevisionMetadata.Image, cache)

		if err != nil {
			if state.bundleSource != nil && state.bundleSource.CanMaintainWithoutContent(ext, state.revisionStates, state.bundleResolution) {
				l.V(1).Info("bundle content unavailable, maintaining current installation",
					"bundle", state.resolvedRevisionMetadata.Name,
					"version", state.resolvedRevisionMetadata.Version,
					"error", err.Error())
				state.imageFS = nil
				return nil, nil
			}
			setStatusProgressing(ext, wrapErrorWithResolutionInfo(state.resolvedRevisionMetadata.BundleMetadata, err))
			setInstalledStatusFromRevisionStates(ext, state.revisionStates)
			return nil, err
		}

		state.imageFS = imageFS
		return nil, nil
	}
}

// ValidateInstallNamespace verifies that a user-provided spec.namespace exists.
//
// A missing namespace is recoverable — the user can create it — so it is surfaced as a retryable
// error rather than a terminal one: the next reconcile succeeds once the namespace exists.
//
// When spec.namespace is omitted the namespace is system-managed and the renderer creates it, so
// there is nothing to check.
func ValidateInstallNamespace(nsClient corev1client.NamespacesGetter) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		if ext.Spec.Namespace == "" {
			return nil, nil
		}

		l := log.FromContext(ctx)
		l.V(1).Info("validating user-provided namespace exists", "namespace", ext.Spec.Namespace)

		_, err := nsClient.Namespaces().Get(ctx, ext.Spec.Namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			nsErr := fmt.Errorf("namespace %q not found; when set, spec.namespace must reference an existing namespace", ext.Spec.Namespace)
			setStatusProgressing(ext, nsErr)
			return nil, nsErr
		}
		if err != nil {
			return nil, fmt.Errorf("error checking namespace %q: %w", ext.Spec.Namespace, err)
		}
		return nil, nil
	}
}

func ApplyBundle(a Applier) ReconcileStepFunc {
	return func(ctx context.Context, state *reconcileState, ext *ocv1.ClusterExtension) (*ctrl.Result, error) {
		l := log.FromContext(ctx)
		revisionAnnotations := map[string]string{
			labels.BundleNameKey:      state.resolvedRevisionMetadata.Name,
			labels.PackageNameKey:     state.resolvedRevisionMetadata.Package,
			labels.BundleVersionKey:   state.resolvedRevisionMetadata.Version,
			labels.BundleReferenceKey: state.resolvedRevisionMetadata.Image,
		}
		if state.resolvedRevisionMetadata.Release != nil {
			revisionAnnotations[labels.BundleReleaseKey] = *state.resolvedRevisionMetadata.Release
		}
		objLbls := map[string]string{
			labels.OwnerKindKey: ocv1.ClusterExtensionKind,
			labels.OwnerNameKey: ext.GetName(),
		}

		l.Info("applying bundle contents")
		// NOTE: We need to be cautious of eating errors here.
		// We should always return any error that occurs during an
		// attempt to apply content to the cluster. Only when there is
		// a verifiable reason to eat the error (i.e it is recoverable)
		// should an exception be made.
		// The following kinds of errors should be returned up the stack
		// to ensure exponential backoff can occur:
		//   - Permission errors (it is not possible to watch changes to permissions.
		//     The only way to eventually recover from permission errors is to keep retrying).
		rolloutSucceeded, rolloutStatus, err := a.Apply(ctx, state.imageFS, ext, objLbls, revisionAnnotations)

		// Set installed status
		if rolloutSucceeded {
			state.revisionStates = &RevisionStates{Installed: state.resolvedRevisionMetadata}
		} else if err == nil && state.revisionStates.Installed == nil && len(state.revisionStates.RollingOut) == 0 {
			state.revisionStates = &RevisionStates{RollingOut: []*RevisionMetadata{state.resolvedRevisionMetadata}}
		}
		setInstalledStatusFromRevisionStates(ext, state.revisionStates)

		// Refresh source-specific deprecation conditions for the newly running bundle.
		if rolloutSucceeded && state.revisionStates.Installed != nil && state.bundleSource != nil {
			state.bundleSource.SetDeprecationStatus(ext, state.revisionStates.Installed.Name, state.bundleResolution)
		}

		// If there was an error applying the resolved bundle,
		// report the error via the Progressing condition.
		if err != nil {
			setStatusProgressing(ext, wrapErrorWithResolutionInfo(state.resolvedRevisionMetadata.BundleMetadata, err))
			return nil, err
		} else if !rolloutSucceeded {
			apimeta.SetStatusCondition(&ext.Status.Conditions, metav1.Condition{
				Type:               ocv1.TypeProgressing,
				Status:             metav1.ConditionTrue,
				Reason:             ocv1.ReasonRollingOut,
				Message:            rolloutStatus,
				ObservedGeneration: ext.GetGeneration(),
			})
		} else {
			setStatusProgressing(ext, nil)
		}
		return nil, nil
	}
}
