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
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

const (
	finalizerName = "undyingproxies.proxy.sfact.io/finalizer"
)

var (
	// tcpForwarders is a map of name-to-TCPForwarder to track globally.
	tcpForwarders = make(map[string]*TCPForwarder)
	// tcpForwardersMux is a mutex lock for modifying forwarders
	tcpForwardersMux = &sync.Mutex{}
	// udpForwarders is a map of name-to-UDPForwarder to track globally.
	udpForwarders = make(map[string]*UDPForwarder)
	// udpForwardersMux is a mutex lock for modifying forwarders.
	udpForwardersMux = &sync.Mutex{}
	patchOptions     = &client.PatchOptions{FieldManager: "undying-proxy-operator"}
)

// custom metrics
var (
	mTCPForwardersRunning = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "undying_proxy_tcp_forwarders",
			Help: "Number of TCP forwarders currently running",
		},
	)

	mTCPFromClientsToTargetBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_tcp_from_clients_to_target_bytes_total",
			Help: "Number of TCP bytes forwarded from clients to the target",
		},
		[]string{"forwarder"},
	)

	mTCPFromTargetToClientsBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_tcp_from_target_to_clients_bytes_total",
			Help: "Number of TCP bytes forwarded from the target back to the clients",
		},
		[]string{"forwarder"},
	)

	mTCPForwardingErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_tcp_forwarding_errors_total",
			Help: "Number of networking-related forwarding errors encountered by the UnDyingProxy controller",
		},
		[]string{"type"},
	)

	mUDPForwardersRunning = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "undying_proxy_udp_forwarders",
			Help: "Number of UDP forwarders currently running",
		},
	)

	mUDPClientsConnected = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "undying_proxy_udp_clients_connected",
			Help: "Number of UDP clients currently connected, by forwarder",
		},
		[]string{"forwarder"},
	)

	mUDPFromClientsToTargetBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_udp_from_clients_to_target_bytes_total",
			Help: "Number of UDP bytes forwarded from clients to the target",
		},
		[]string{"forwarder"},
	)

	mUDPFromTargetToClientsBytesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_udp_from_target_to_clients_bytes_total",
			Help: "Number of UDP bytes forwarded from the target back to the clients",
		},
		[]string{"forwarder"},
	)

	mUDPForwardingErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_udp_forwarding_errors_total",
			Help: "Number of networking-related forwarding errors encountered by the UnDyingProxy controller",
		},
		[]string{"type"},
	)

	// errors
	mOperatorErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_operator_errors_total",
			Help: "Number of operator-related errors encountered by the UnDyingProxy controller",
		},
		[]string{"type"},
	)
)

// UnDyingProxyReconciler reconciles a UnDyingProxy object
type UnDyingProxyReconciler struct {
	client.Client
	Scheme             *runtime.Scheme
	UDPServiceToManage string
	TCPServiceToManage string
	UDPBufferBytes     int
	log                logr.Logger
}

// TCPForwarder tracks the TCP listener for an UnDyingProxy
type TCPForwarder struct {
	name      string
	closeChan chan struct{}
	listener  net.Listener
	log       logr.Logger
}

// UDPForwarder tracks the UDP listener and connections for an UnDyingProxy
type UDPForwarder struct {
	name               string
	closeChan          chan struct{}
	listenAddress      *net.UDPAddr
	targetAddress      *net.UDPAddr
	listenerConnection *net.UDPConn
	clients            map[string]*UDPClient
	clientsMux         *sync.Mutex
	writeTimeout       time.Duration
	readTimeout        time.Duration
	udpBufferBytes     int
	log                logr.Logger
}

// UDPClient tracks a client and target connection
type UDPClient struct {
	// clientAddress is the IP address to the client, e.g. the game client
	clientAddress *net.UDPAddr
	// targetConnection is the connection to the target, e.g. the game server
	targetConnection *net.UDPConn
	log              logr.Logger
}

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		mTCPForwardersRunning,
		mTCPFromClientsToTargetBytesTotal,
		mTCPFromTargetToClientsBytesTotal,
		mUDPForwardersRunning,
		mUDPClientsConnected,
		mUDPFromClientsToTargetBytesTotal,
		mUDPFromTargetToClientsBytesTotal,
		mTCPForwardingErrorsTotal,
	)
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
	r.log = crlog.FromContext(ctx)
	log := r.log

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

	r.log.V(1).Info("UnDyingProxy is being deleted, performing finalizer operations")

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

	r.log.V(1).Info("UnDyingProxy deleted, finished finalizer operations")

	// end reconciliation
	return true, ctrl.Result{}, nil
}

func (r *UnDyingProxyReconciler) handleTCPCleanup(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := r.log.WithValues("protocol", "TCP")

	tcpForwardersMux.Lock()
	forwarder, ok := tcpForwarders[unDyingProxy.Name]
	if !ok {
		log.V(1).Info("TCP Forwarder was already stopped")
	} else {
		go func() {
			forwarder.closeChan <- struct{}{}
		}()
		err := forwarder.listener.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close TCP listener on forwarder shutdown")
		}
		log.V(1).Info("TCP Forwarder is now stopped")
	}
	tcpForwardersMux.Unlock()

	if r.TCPServiceToManage != "" {
		err := r.cleanupServiceForUnDyingProxy(ctx, unDyingProxy, v1.ProtocolTCP)
		if err != nil {
			log.Error(err, "Failed to cleanup Service for UnDyingProxy")
			mOperatorErrorsTotal.WithLabelValues("FailedServiceCleanup").Inc()
			return true, ctrl.Result{}, err
		}
		log.V(1).Info("Service for UnDyingProxy is now cleaned up")
	}

	return false, ctrl.Result{}, nil
}

func (r *UnDyingProxyReconciler) handleUDPCleanup(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := r.log.WithValues("protocol", "UDP")

	udpForwardersMux.Lock()
	forwarder, found := udpForwarders[unDyingProxy.Name]
	if !found {
		log.V(1).Info("UDP Forwarder was already stopped")
	} else {
		go func() { forwarder.closeChan <- struct{}{} }()
		err := forwarder.listenerConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close UDP listenerConnection")
		}
		log.V(1).Info("UDP Forwarder is now stopped")
	}
	udpForwardersMux.Unlock()

	if r.UDPServiceToManage != "" {
		err := r.cleanupServiceForUnDyingProxy(ctx, unDyingProxy, v1.ProtocolUDP)
		if err != nil {
			log.Error(err, "Failed to cleanup Service for UnDyingProxy")
			mOperatorErrorsTotal.WithLabelValues("FailedServiceCleanup").Inc()
			return true, ctrl.Result{}, err
		}
		log.V(1).Info("Service for UnDyingProxy is now cleaned up")
	}

	return false, ctrl.Result{}, nil
}

func (r *UnDyingProxyReconciler) removeFinalizer(
	ctx context.Context,
	req ctrl.Request,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
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
		mOperatorErrorsTotal.WithLabelValues("FailedRemoveFinalizer").Inc()
		r.log.Error(err, "Failed to remove finalizer")
		return true, ctrl.Result{}, err
	}

	return false, ctrl.Result{}, nil
}

// handleFinalizer handles setting the finalizer for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleFinalizer(
	ctx context.Context,
	req ctrl.Request,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := r.log
	if controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
		return false, ctrl.Result{}, nil
	}

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
		mOperatorErrorsTotal.WithLabelValues("FailedAddFinalizer").Inc()
		log.Error(err, "Failed to add finalizer")
		return true, ctrl.Result{}, err
	}

	// continue reconciliation
	return false, ctrl.Result{}, nil
}

// handleTCP handles the TCP configuration for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleTCP(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := r.log.WithValues("protocol", "TCP")

	tcpForwardersMux.Lock()
	_, ok := tcpForwarders[unDyingProxy.Name]
	if ok {
		log.V(1).Info("Forwarder already running")
	} else {
		forwarder, err := setupTCPForwarder(unDyingProxy)
		if err != nil {
			mOperatorErrorsTotal.WithLabelValues("FailedSetupTCPForwarder").Inc()
			log.Error(err, "Failed to setup Forwarder")
			return true, ctrl.Result{}, err
		}
		log.V(1).Info("Forwarder is now running")
		tcpForwarders[unDyingProxy.Name] = forwarder
	}
	tcpForwardersMux.Unlock()

	if r.TCPServiceToManage != "" {
		err := r.manageServiceForUnDyingProxy(ctx, unDyingProxy, v1.ProtocolTCP)
		if err != nil {
			mOperatorErrorsTotal.WithLabelValues("FailedManageTCPService").Inc()
			log.Error(err, "Failed to manage Service for UnDyingProxy")
			return true, ctrl.Result{}, err
		}

		log.V(1).Info("Service for UnDyingProxy is up to date")
	}

	return false, ctrl.Result{}, nil
}

// handleUDP handles the UDP configuration for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleUDP(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := r.log.WithValues("protocol", "UDP")

	// TODO move this to object validation
	if r.UDPBufferBytes == 0 {
		return true, ctrl.Result{}, errors.New("UDPBufferBytes must be non-zero")
	}

	udpForwardersMux.Lock()
	_, ok := udpForwarders[unDyingProxy.Name]
	if ok {
		log.V(1).Info("UDP Forwarder already running")
	} else {
		forwarder, err := setupUDPForwarder(unDyingProxy, r.UDPBufferBytes)
		if err != nil {
			udpForwardersMux.Unlock()
			mOperatorErrorsTotal.WithLabelValues("FailedSetupUDPForwarder").Inc()
			log.Error(err, "Failed to setup Forwarder")
			return true, ctrl.Result{}, err
		}
		log.V(1).Info("UDP Forwarder is now running")
		udpForwarders[unDyingProxy.Name] = forwarder
	}
	udpForwardersMux.Unlock()

	if r.UDPServiceToManage != "" {
		err := r.manageServiceForUnDyingProxy(ctx, unDyingProxy, v1.ProtocolUDP)
		if err != nil {
			mOperatorErrorsTotal.WithLabelValues("FailedManageUDPService").Inc()
			log.Error(err, "Failed to manage Service for UnDyingProxy")
			return true, ctrl.Result{}, err
		}

		log.V(1).Info("Service for UnDyingProxy is up to date")
	}

	return false, ctrl.Result{}, nil
}

// setupTCPForwarder sets up a TCP forwarder for an UnDyingProxy
// runs once per UnDyingProxy
func setupTCPForwarder(unDyingProxy *proxyv1alpha1.UnDyingProxy) (*TCPForwarder, error) {
	listenAddr := fmt.Sprintf(":%d", unDyingProxy.Spec.TCP.ListenPort)
	targetAddr := fmt.Sprintf("%s:%d", unDyingProxy.Spec.TCP.TargetHost, unDyingProxy.Spec.TCP.TargetPort)

	log := ctrl.Log.WithValues(
		"name", unDyingProxy.Name,
		"protocol", "TCP",
		"listen", listenAddr,
		"target", targetAddr,
	)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to setup listener: %w", err)
	}

	forwarder := &TCPForwarder{
		name:      unDyingProxy.Name,
		closeChan: make(chan struct{}),
		listener:  listener,
		log:       log,
	}

	go acceptTCPConnections(forwarder, listener, targetAddr)
	mTCPForwardersRunning.Inc()

	return forwarder, nil
}

// acceptTCPConnections listens for new TCP connections and
// sets up goroutines to forward them.
func acceptTCPConnections(
	forwarder *TCPForwarder,
	listener net.Listener,
	targetAddr string,
) {
	log := forwarder.log

	defer shutdownAcceptTCPConnections(forwarder, listener)

	for {
		select {
		case <-forwarder.closeChan:
			log.V(1).Info("Shutting down listener")
			return
		default:
			// connToListener represents a new connection to the listener from a client
			connToListener, err := listener.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					continue
				}
				mTCPForwardingErrorsTotal.WithLabelValues("Accept").Inc()
				log.Error(err, "Failed to Accept on listener")
				continue
			}
			go forwardTCPConnection(forwarder, connToListener, targetAddr)
		}
	}
}

func shutdownAcceptTCPConnections(
	forwarder *TCPForwarder,
	listener net.Listener,
) {
	log := forwarder.log

	tcpForwardersMux.Lock()

	err := listener.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Error(err, "Failed to close listener")
	} else {
		log.V(1).Info("Listener Closed")
	}

	delete(tcpForwarders, forwarder.name)
	tcpForwardersMux.Unlock()

	close(forwarder.closeChan)
	mTCPForwardersRunning.Dec()
}

// forwardTCPConnection forwards TCP connections from clients to a target server, and back
func forwardTCPConnection(
	forwarder *TCPForwarder,
	connToListener net.Conn,
	targetAddr string,
) {
	log := forwarder.log

	// connToTarget represents a connection to the target server
	connToTarget, err := net.Dial("tcp", targetAddr)
	if err != nil {
		mTCPForwardingErrorsTotal.WithLabelValues("DialServerTarget").Inc()
		log.Error(err, "Dial failed")
		return
	}
	defer func() {
		err = connToTarget.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close target connection")
		}
	}()

	// fromClientToTarget
	go func() {
		defer func() {
			err = connToListener.Close()
			if err != nil && !errors.Is(err, net.ErrClosed) {
				log.Error(err, "Failed to close listener connection")
			}
		}()
		numBytesCopied, err := io.Copy(connToTarget, connToListener)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				mTCPForwardingErrorsTotal.WithLabelValues("CopyClientToTarget").Inc()
				log.V(1).Error(err, "Failed to copy data from client to target")
			}
		}
		mTCPFromClientsToTargetBytesTotal.WithLabelValues(forwarder.name).Add(float64(numBytesCopied))
	}()

	// fromTargetToClient
	numBytesCopied, err := io.Copy(connToListener, connToTarget)
	if err != nil {
		mTCPForwardingErrorsTotal.WithLabelValues("CopyTargetToClient").Inc()
		log.V(2).Error(err, "Failed to copy data from target to client")
	}
	mTCPFromTargetToClientsBytesTotal.WithLabelValues(forwarder.name).Add(float64(numBytesCopied))
}

// setupUDPForwarder sets up a UDP forwarder for an UnDyingProxy
// runs once per UnDyingProxy
func setupUDPForwarder(
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	udpBufferBytes int,
) (*UDPForwarder, error) {
	// TODO re-resolve DNS occasionally or on error

	listenAddressStr := fmt.Sprintf(":%d", unDyingProxy.Spec.UDP.ListenPort)
	listenAddress, err := net.ResolveUDPAddr("udp", listenAddressStr)
	if err != nil {
		mUDPForwardingErrorsTotal.WithLabelValues("ResolveListenAddress").Inc()
		return nil, fmt.Errorf("failed to resolve listen address: %w", err)
	}

	targetAddressStr := fmt.Sprintf("%s:%d", unDyingProxy.Spec.UDP.TargetHost, unDyingProxy.Spec.UDP.TargetPort)
	targetAddress, err := net.ResolveUDPAddr("udp", targetAddressStr)
	if err != nil {
		mUDPForwardingErrorsTotal.WithLabelValues("ResolveTargetAddress").Inc()
		return nil, fmt.Errorf("failed to resolve target address: %w", err)
	}

	log := ctrl.Log.WithValues(
		"name", unDyingProxy.Name,
		"protocol", "UDP",
	)

	listenerConnection, err := net.ListenUDP("udp", listenAddress)
	if err != nil {
		mUDPForwardingErrorsTotal.WithLabelValues("Listen").Inc()
		return nil, fmt.Errorf("failed to listen on port %d: %w", unDyingProxy.Spec.UDP.ListenPort, err)
	}

	readTimeoutSeconds := unDyingProxy.Spec.UDP.ReadTimeoutSeconds
	// default to 30 seconds
	if readTimeoutSeconds == 0 {
		readTimeoutSeconds = 30
	}
	writeTimeoutSeconds := unDyingProxy.Spec.UDP.WriteTimeoutSeconds
	// default to 5 seconds
	if writeTimeoutSeconds == 0 {
		writeTimeoutSeconds = 5
	}

	forwarder := &UDPForwarder{
		name:               unDyingProxy.Name,
		closeChan:          make(chan struct{}),
		listenAddress:      listenAddress,
		targetAddress:      targetAddress,
		listenerConnection: listenerConnection,
		writeTimeout:       time.Duration(writeTimeoutSeconds) * time.Second,
		readTimeout:        time.Duration(readTimeoutSeconds) * time.Second,
		clients:            make(map[string]*UDPClient),
		clientsMux:         &sync.Mutex{},
		udpBufferBytes:     udpBufferBytes,
		log:                log,
	}

	go forwardUDPClientsToTarget(forwarder)
	mUDPForwardersRunning.Inc()

	return forwarder, nil
}

// forwardUDPClientsToTarget handles forwarding UDP packets from clients to the target.
// this goroutine stays running for the lifetime of the forwarder, and for each client that connects
// it sets up an additional goroutine to forward packets from the target back to that client.
// each client is tracked in the forwarder.clients map, and self-cleans up when experiencing a read or write timeout.
func forwardUDPClientsToTarget(forwarder *UDPForwarder) {
	log := forwarder.log.WithValues(
		"direction", "fromClientsToTarget",
		"listen", forwarder.listenAddress.String(),
		"target", forwarder.targetAddress.String(),
	)

	defer shutdownUDPClientsToTargetForwarder(forwarder)

	data := make([]byte, forwarder.udpBufferBytes)

	// this loops runs once for every packet forwarded from a client to the target
	for {
		select {
		case <-forwarder.closeChan:
			return
		default:
			// block indefinitely until a packet is received
			numBytesRead, clientAddr, readErr := forwarder.listenerConnection.ReadFromUDP(data)

			// track each client that connects to the listenAddress by clientAddrStr
			clientAddrStr := clientAddr.String()

			// 'Callers should always process the n > 0 bytes returned before considering the error err.'
			if numBytesRead > 0 {
				udpClient, found := forwarder.clients[clientAddrStr]
				if !found {
					log.V(2).Info("New client connected", "client", clientAddr.String())
					var err error
					udpClient, err = setupUDPClient(forwarder, clientAddr)
					if err != nil {
						log.V(1).Error(err, "Error connecting to target")
						// continue sending more packets when the target can't be dialed
						continue
					}
				}

				err := udpClient.targetConnection.SetWriteDeadline(time.Now().Add(forwarder.writeTimeout))
				if err != nil {
					mUDPForwardingErrorsTotal.WithLabelValues("SetWriteDeadlineClientToTarget").Inc()
					log.Error(err, "Error setting write deadline client to target")
				}

				numBytesWritten, err := udpClient.targetConnection.Write(data[:numBytesRead])
				if err != nil {
					mUDPForwardingErrorsTotal.WithLabelValues("WriteToTarget").Inc()
					log.V(1).Error(err, "Error writing to target")
					continue
				}

				mUDPFromClientsToTargetBytesTotal.WithLabelValues(forwarder.name).Add(float64(numBytesWritten))
			}

			if readErr != nil {
				// ignore if listener is closed, this happens all the time from timeouts and is normal
				if !errors.Is(readErr, net.ErrClosed) {
					mUDPForwardingErrorsTotal.WithLabelValues("ReadFromClient").Inc()
					log.V(1).Error(readErr, "reading from listener")
				}
			}
		}
	}
}

func shutdownUDPClientsToTargetForwarder(forwarder *UDPForwarder) {
	log := forwarder.log

	udpForwardersMux.Lock()

	forwarder.clientsMux.Lock()
	for _, udpClient := range forwarder.clients {
		err := udpClient.targetConnection.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close UDP client's targetConnection")
		}
		log.V(2).Info("Closed UDP client's targetConnection: " + udpClient.clientAddress.String())
	}
	forwarder.clientsMux.Unlock()

	delete(udpForwarders, forwarder.name)
	udpForwardersMux.Unlock()
	mUDPForwardersRunning.Dec()

	log.V(1).Info("UDP Forwarder is now stopped")
}

func setupUDPClient(
	forwarder *UDPForwarder,
	clientAddr *net.UDPAddr,
) (*UDPClient, error) {
	log := forwarder.log.WithValues(
		"target", forwarder.targetAddress.String(),
		"client", clientAddr.String(),
	)

	forwarder.clientsMux.Lock()
	defer forwarder.clientsMux.Unlock()

	targetConnection, err := net.DialUDP("udp", nil, forwarder.targetAddress)
	if err != nil {
		mUDPForwardingErrorsTotal.WithLabelValues("DialUDPTarget").Inc()
		return nil, err
	}

	udpClient := &UDPClient{
		clientAddress:    clientAddr,
		targetConnection: targetConnection,
		log:              log,
	}
	forwarder.clients[clientAddr.String()] = udpClient
	mUDPClientsConnected.WithLabelValues(forwarder.name).Inc()

	go forwardUDPTargetToClient(forwarder, udpClient)

	return udpClient, nil
}

// forwardUDPTargetToClient handles forwarding packets from the target back to a specific client
func forwardUDPTargetToClient(forwarder *UDPForwarder, udpClient *UDPClient) {
	log := udpClient.log.WithValues("direction", "fromTargetToClient")

	defer shutdownUDPTargetToClientForwarder(forwarder, udpClient)

	data := make([]byte, forwarder.udpBufferBytes)

	// this loop runs once for every packet sent from the target to the client
	for {
		// set read deadline so this goroutine will eventually shutdown
		err := udpClient.targetConnection.SetReadDeadline(time.Now().Add(forwarder.readTimeout))
		if err != nil {
			mUDPForwardingErrorsTotal.WithLabelValues("SetReadDeadlineTargetToClient").Inc()
			log.Error(err, "Error setting read deadline")
			return
		}

		// block until packet received, deadline reached, or connection closed
		numBytesRead, _, readErr := udpClient.targetConnection.ReadFromUDP(data)
		// 'Callers should always process the n > 0 bytes returned before considering the error err.'
		if numBytesRead > 0 {
			err = forwarder.listenerConnection.SetWriteDeadline(time.Now().Add(forwarder.writeTimeout))
			if err != nil {
				mUDPForwardingErrorsTotal.WithLabelValues("SetWriteDeadlineTargetToClient").Inc()
				log.Error(err, "Error setting write deadline")
				return
			}
			numBytesWritten, err := forwarder.listenerConnection.WriteToUDP(data[:numBytesRead], udpClient.clientAddress)
			mUDPFromTargetToClientsBytesTotal.WithLabelValues(forwarder.name).Add(float64(numBytesWritten))
			if err != nil {
				mUDPForwardingErrorsTotal.WithLabelValues("WriteToClient").Inc()
				log.Error(err, "Error writing to client")
				return
			}
		}

		if readErr != nil {
			if !errors.Is(err, net.ErrClosed) && !errors.Is(readErr, os.ErrDeadlineExceeded) {
				mUDPForwardingErrorsTotal.WithLabelValues("ReadFromTarget").Inc()
				log.Error(readErr, "Error reading from target")
			}

			return
		}
	}
}

func shutdownUDPTargetToClientForwarder(
	forwarder *UDPForwarder,
	udpClient *UDPClient,
) {
	log := udpClient.log

	forwarder.clientsMux.Lock()

	err := udpClient.targetConnection.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Error(err, "Error closing UDP targetConnection")
	}

	delete(forwarder.clients, udpClient.clientAddress.String())
	forwarder.clientsMux.Unlock()

	log.V(2).Info("Cleaned up UDP client: " + udpClient.clientAddress.String())

	mUDPClientsConnected.WithLabelValues(forwarder.name).Dec()
}

func (r *UnDyingProxyReconciler) manageServiceForUnDyingProxy(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	protocol v1.Protocol,
) error {
	protocolStr := "TCP"
	serviceName := r.TCPServiceToManage
	listenPort := unDyingProxy.Spec.TCP.ListenPort
	if protocol == v1.ProtocolUDP {
		protocolStr = "UDP"
		serviceName = r.UDPServiceToManage
		listenPort = unDyingProxy.Spec.UDP.ListenPort
	}
	log := r.log.WithValues(
		"protocol", protocolStr,
		"Service.Name", serviceName,
		"listenPort", listenPort,
	)

	namespacedName := types.NamespacedName{Name: serviceName, Namespace: unDyingProxy.Namespace}
	service := &v1.Service{}
	if err := r.Get(ctx, namespacedName, service); err != nil {
		return err
	}

	for _, existingPort := range service.Spec.Ports {
		if existingPort.Name == unDyingProxy.Name &&
			existingPort.Port == int32(listenPort) &&
			// targetPort is listenPort because this app is the target
			existingPort.TargetPort == intstr.FromInt(listenPort) &&
			existingPort.Protocol == protocol {
			log.V(1).Info("Service already up to date")

			return nil
		}
	}

	err := r.updateService(
		ctx,
		namespacedName,
		service,
		listenPort,
		protocol,
		unDyingProxy.Name,
	)
	if err != nil {
		return err
	}

	log.V(1).Info("Updated Service with listenPort")

	return nil
}

func (r *UnDyingProxyReconciler) updateService(
	ctx context.Context,
	namespacedName types.NamespacedName,
	service *v1.Service,
	listenPort int,
	protocol v1.Protocol,
	unDyingProxyName string,
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		exists := false
		for index, existingPort := range serviceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxyName {
				exists = true
				serviceCopy.Spec.Ports[index].Port = int32(listenPort)
				serviceCopy.Spec.Ports[index].TargetPort = intstr.FromInt(listenPort)
				serviceCopy.Spec.Ports[index].Protocol = protocol
				break
			}
		}

		if !exists {
			portSpec := v1.ServicePort{
				Name:       unDyingProxyName,
				Port:       int32(listenPort),
				TargetPort: intstr.FromInt(listenPort),
				Protocol:   protocol,
			}
			serviceCopy.Spec.Ports = append(serviceCopy.Spec.Ports, portSpec)
		}

		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("failed to update (%s) Service to set Port: (%w)", service.Name, err)
	}

	return nil
}

func (r *UnDyingProxyReconciler) cleanupServiceForUnDyingProxy(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	protocol v1.Protocol,
) error {
	protocolStr := "TCP"
	serviceName := r.TCPServiceToManage
	listenPort := unDyingProxy.Spec.TCP.ListenPort
	if protocol == v1.ProtocolUDP {
		protocolStr = "UDP"
		serviceName = r.UDPServiceToManage
		listenPort = unDyingProxy.Spec.UDP.ListenPort
	}

	log := r.log.WithValues(
		"protocol", protocolStr,
		"Service.Name", serviceName,
		"listenPort", listenPort,
	)

	service := &v1.Service{}
	namespacedName := types.NamespacedName{Name: serviceName, Namespace: unDyingProxy.Namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		for index, existingPort := range serviceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxy.Name {
				serviceCopy.Spec.Ports = append(
					serviceCopy.Spec.Ports[:index],
					serviceCopy.Spec.Ports[index+1:]...)
				break
			}
		}

		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("failed to update (%s) Service to remove Port: (%w)", service.Name, err)
	}

	log.V(1).Info("Updated Service to remove listenPort")

	return nil
}
