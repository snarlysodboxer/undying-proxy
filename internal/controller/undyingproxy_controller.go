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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

// ctxLogger is the context key type for the logger
type ctxLogger struct{}

var patchOptions = &client.PatchOptions{FieldManager: "undying-proxy-operator"}

// custom metrics - MOVED TO internal/controller/metrics.go

// UnDyingProxyReconciler reconciles a UnDyingProxy object
type UnDyingProxyReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	UDPServiceToManage string
	TCPServiceToManage string
	UDPBufferBytes     int
}

// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",namespace=undying-proxy,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=undying-proxy,resources=services/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *UnDyingProxyReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	log := crlog.FromContext(ctx)
	ctx = context.WithValue(ctx, ctxLogger{}, log) // Add logger to context value

	// get UnDyingProxy object
	unDyingProxy := &proxyv1alpha1.UnDyingProxy{}
	if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		if apierrors.IsNotFound(err) {
			log.V(2).Info("Ignoring UnDyingProxy not found error")
			return ctrl.Result{}, nil
		}

		mOperatorErrorsTotal.WithLabelValues("FailedFetch").Inc()
		log.Error(err, "Failed to fetch UnDyingProxy")
		return ctrl.Result{}, err
	}

	doReturn, result, err := r.handleDeletion(ctx, req, unDyingProxy)
	if err != nil || doReturn {
		return result, err
	}

	doReturn, result, err = r.handleFinalizer(ctx, req, unDyingProxy)
	if err != nil || doReturn {
		return result, err
	}

	if unDyingProxy.Spec.TCP != nil {
		doReturn, result, err = r.handleTCP(ctx, unDyingProxy)
		if err != nil || doReturn {
			return result, err
		}
	}

	if unDyingProxy.Spec.UDP != nil {
		doReturn, result, err = r.handleUDP(ctx, unDyingProxy)
		if err != nil || doReturn {
			return result, err
		}
	}

	// Set the ready status if it's not already
	if unDyingProxy.Status.Ready {
		return ctrl.Result{}, nil
	}
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
			return err
		}
		unDyingProxy.Status.Ready = true
		return r.Status().Update(ctx, unDyingProxy)
	})
	if err != nil {
		mOperatorErrorsTotal.WithLabelValues("FailedUpdateStatus").Inc()
		log.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UnDyingProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.UnDyingProxy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		Complete(r)
}

// handleDeletion handles the deletion of an UnDyingProxy
func (r *UnDyingProxyReconciler) handleDeletion(
	ctx context.Context,
	req ctrl.Request,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger)
	if unDyingProxy.DeletionTimestamp.IsZero() {
		// continue reconciliation
		return false, ctrl.Result{}, nil
	}

	// handle the rare case where it's being deleted but the finalizer is not present
	if !controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
		// end reconciliation
		return true, ctrl.Result{}, nil
	}

	// confirm that the UnDyingProxy still exists
	err := r.Get(ctx, req.NamespacedName, unDyingProxy)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// end reconciliation
			return true, ctrl.Result{}, nil
		}
		mOperatorErrorsTotal.WithLabelValues("FailedFetch").Inc()
		return true, ctrl.Result{}, err
	}

	log.V(1).Info("UnDyingProxy is being deleted, performing finalizer operations")

	if unDyingProxy.Spec.TCP != nil {
		doReturn, result, err := r.handleTCPCleanup(ctx, unDyingProxy)
		if err != nil || doReturn {
			return doReturn, result, err
		}
	}

	if unDyingProxy.Spec.UDP != nil {
		doReturn, result, err := r.handleUDPCleanup(ctx, unDyingProxy)
		if err != nil || doReturn {
			return doReturn, result, err
		}
	}

	doReturn, result, err := r.removeFinalizer(ctx, req, unDyingProxy)
	if err != nil || doReturn {
		return doReturn, result, err
	}

	log.V(1).Info("UnDyingProxy deleted, finished finalizer operations")

	// end reconciliation
	return true, ctrl.Result{}, nil
}
