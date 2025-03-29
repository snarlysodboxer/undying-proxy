/*
Copyright 2024 david amick.

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

package controller

import (
	"context"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

const (
	finalizerName = "undyingproxies.proxy.sfact.io/finalizer"
)

func (r *UnDyingProxyReconciler) removeFinalizer(
	ctx context.Context,
	req ctrl.Request,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger)
	log.V(1).Info("Removing finalizer")

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := r.Get(ctx, req.NamespacedName, unDyingProxy)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}
			return err
		}
		removed := controllerutil.RemoveFinalizer(unDyingProxy, finalizerName)
		if !removed {
			return nil
		}
		err = r.Update(ctx, unDyingProxy)
		if err != nil && apierrors.IsNotFound(err) {
			return nil
		}

		return err
	})
	if err != nil {
		log.Error(err, "Failed to remove finalizer")
		mOperatorErrorsTotal.WithLabelValues("FailedRemoveFinalizer").Inc()
		return true, ctrl.Result{}, err
	}

	log.V(1).Info("Removed finalizer")

	return false, ctrl.Result{}, nil
}

// handleFinalizer handles setting the finalizer for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleFinalizer(
	ctx context.Context,
	req ctrl.Request,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger)
	if controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
		return false, ctrl.Result{}, nil
	}

	log.V(1).Info("Adding finalizer")
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
			return err
		}
		// AddFinalizer only adds the finalizer if it doesn't already exist
		if added := controllerutil.AddFinalizer(unDyingProxy, finalizerName); added {
			return r.Update(ctx, unDyingProxy)
		}
		return nil
	})
	if err != nil {
		log.Error(err, "Failed to add finalizer")
		mOperatorErrorsTotal.WithLabelValues("FailedAddFinalizer").Inc()
		return true, ctrl.Result{}, err
	}
	log.V(1).Info("Finalizer added")

	// continue reconciliation
	return false, ctrl.Result{}, nil
}
