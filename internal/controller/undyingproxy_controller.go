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
	"sync"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

const (
	finalizerName = "undyingproxies.proxy.sfact.io/finalizer"
)

var (
	// tcpForwarders is a map of name-to-TCPForwarder to track globally in memory
	tcpForwarders = make(map[string]*TCPForwarder)
	// tcpForwardersMux is a mutex lock for modifying forwarders
	tcpForwardersMux = &sync.Mutex{}
	// udpForwarders is a map of name-to-UDPForwarder to track globally in memory
	udpForwarders = make(map[string]*UDPForwarder)
	// udpForwardersMux is a mutex lock for modifying forwarders
	udpForwardersMux = &sync.Mutex{}
	patchOptions     = &client.PatchOptions{FieldManager: "undying-proxy-operator"}
)

// custom metrics
var (
	// TCP
	tcpForwardersRunningM = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "undying_proxy_tcp_forwarders",
			Help: "Number of TCP forwarders currently running",
		},
	)

	tcpFromClientsToTargetBytesTotalM = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_tcp_from_clients_to_target_bytes_total",
			Help: "Number of TCP bytes forwarded from clients to the target",
		},
		[]string{"forwarder"},
	)

	tcpFromTargetToClientsBytesTotalM = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_tcp_from_target_to_clients_bytes_total",
			Help: "Number of TCP bytes forwarded from the target back to the clients",
		},
		[]string{"forwarder"},
	)

	// UDP
	udpForwardersRunningM = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "undying_proxy_udp_forwarders",
			Help: "Number of UDP forwarders currently running",
		},
	)

	udpClientsConnectedM = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "undying_proxy_udp_clients_connected",
			Help: "Number of UDP clients currently connected, by forwarder",
		},
		[]string{"forwarder"},
	)

	udpFromClientsToTargetBytesTotalM = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_udp_from_clients_to_target_bytes_total",
			Help: "Number of UDP bytes forwarded from clients to the target",
		},
		[]string{"forwarder"},
	)

	udpFromTargetToClientsBytesTotalM = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_udp_from_target_to_clients_bytes_total",
			Help: "Number of UDP bytes forwarded from the target back to the clients",
		},
		[]string{"forwarder"},
	)

	// errors
	operatorErrorsTotalM = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_operator_errors_total",
			Help: "Number of operator-related errors encountered by the UnDyingProxy controller",
		},
		[]string{"type"},
	)

	forwardingErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "undying_proxy_forwarding_errors_total",
			Help: "Number of networking-related forwarding errors encountered by the UnDyingProxy controller",
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
}

// TCPForwarder tracks the TCP listener for an UnDyingProxy
type TCPForwarder struct {
	name      string
	closeChan chan struct{}
	listener  net.Listener
}

// UDPForwarder tracks the UDP listener and connections for an UnDyingProxy
type UDPForwarder struct {
	name string
	// closeChan is for closing the targetForwarder goroutine, which will in turn close the client connections
	closeChan          chan struct{}
	listenAddress      *net.UDPAddr
	targetAddress      *net.UDPAddr
	listenerConnection *net.UDPConn
	clients            map[string]*UDPClient
	clientsMux         *sync.Mutex
	targetReadTimeout  time.Duration
	udpBufferBytes     int
}

// UDPClient tracks a client and target connection
type UDPClient struct {
	// closeChan is for closing the targetForwarder goroutine
	closeChan chan struct{}

	// clientConnection is the connection to the client, e.g. the game client
	clientAddress *net.UDPAddr
	// targetConnection is the connection to the target, e.g. the game server
	targetConnection *net.UDPConn
}

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		tcpForwardersRunningM,
		tcpFromClientsToTargetBytesTotalM,
		tcpFromTargetToClientsBytesTotalM,
		udpForwardersRunningM,
		udpClientsConnectedM,
		udpFromClientsToTargetBytesTotalM,
		udpFromTargetToClientsBytesTotalM,
		forwardingErrorsTotal,
	)
}

// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=proxy.sfact.io,namespace=undying-proxy,resources=undyingproxies/finalizers,verbs=update
// +kubebuilder:rbac:groups="",namespace=undying-proxy,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",namespace=undying-proxy,resources=services/status,verbs=get

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *UnDyingProxyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	gLog := log.FromContext(ctx)

	// get UnDyingProxy object
	unDyingProxy := &proxyv1alpha1.UnDyingProxy{}
	if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
		// we'll ignore not-found errors, since they can't be fixed by an immediate
		// requeue (we'll need to wait for a new notification), and we can get them
		// on deleted requests.
		if apierrors.IsNotFound(err) {
			gLog.V(2).Info("Ignoring UnDyingProxy not found error")
			return ctrl.Result{}, nil
		}

		forwardingErrorsTotal.WithLabelValues("FailedFetch").Inc()
		gLog.Error(err, "Failed to fetch UnDyingProxy")
		return ctrl.Result{}, err
	}

	// Handle the UnDyingProxy being deleted
	if !unDyingProxy.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle the rare case where it's being deleted but the finalizer is not present
		if !controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
			// end reconciliation
			return ctrl.Result{}, nil
		}

		// Confirm that the UnDyingProxy still exists
		err := r.Get(ctx, req.NamespacedName, unDyingProxy)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			forwardingErrorsTotal.WithLabelValues("FailedFetch").Inc()
			return ctrl.Result{}, err
		}

		gLog.V(1).Info("UnDyingProxy is being deleted, performing Finalizer Operations")

		// TCP
		if unDyingProxy.Spec.TCP != nil {
			fLog := gLog.WithValues("protocol", "TCP")
			// ensure shutdown of the forwarder
			tcpForwardersMux.Lock()
			forwarder, ok := tcpForwarders[unDyingProxy.Name]
			tcpForwardersMux.Unlock()
			if !ok {
				fLog.V(1).Info("Forwarder is already stopped")
			} else {
				// stop clients-to-target (listener) goroutine
				forwarder.listener.Close()
				forwarder.closeChan <- struct{}{}
				fLog.V(1).Info("Forwarder is now stopped")
			}

			// Optionally manage the Kubernetes Service
			if r.TCPServiceToManage != "" {
				fLog = fLog.WithValues("Service.Name", r.TCPServiceToManage)
				if err := r.cleanupServiceForUnDyingProxy(
					ctx,
					fLog,
					unDyingProxy.Name,
					r.TCPServiceToManage,
					unDyingProxy.Namespace,
					unDyingProxy.Spec.TCP.ListenPort,
				); err != nil {
					fLog.Error(err, "Failed to cleanup Service for UnDyingProxy")
					forwardingErrorsTotal.WithLabelValues("FailedServiceCleanup").Inc()
					return ctrl.Result{}, err
				}
				fLog.V(1).Info("Service for UnDyingProxy is now cleaned up")
			}
		}

		// UDP
		if unDyingProxy.Spec.UDP != nil {
			fLog := gLog.WithValues("protocol", "UDP")
			// ensure shutdown of the forwarder
			udpForwardersMux.Lock()
			forwarder, ok := udpForwarders[unDyingProxy.Name]
			udpForwardersMux.Unlock()
			if !ok {
				fLog.V(1).Info("Forwarder is already stopped")
			} else {
				// stop clients-to-target (listener) goroutine
				go func() {
					forwarder.listenerConnection.Close()
					forwarder.closeChan <- struct{}{}
					fLog.V(1).Info("Forwarder is now stopped")
				}()
			}

			// Optionally manage the Kubernetes Service
			if r.UDPServiceToManage != "" {
				if err := r.cleanupServiceForUnDyingProxy(
					ctx,
					fLog,
					unDyingProxy.Name,
					r.UDPServiceToManage,
					unDyingProxy.Namespace,
					unDyingProxy.Spec.UDP.ListenPort,
				); err != nil {
					fLog.Error(err, "Failed to cleanup Service for UnDyingProxy")
					forwardingErrorsTotal.WithLabelValues("FailedServiceCleanup").Inc()
					return ctrl.Result{}, err
				}
				fLog.V(1).Info("Service for UnDyingProxy is now cleaned up")
			}
		}

		// Remove the finalizer
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
				// ignore not found errors
				if apierrors.IsNotFound(err) {
					return nil
				}
				return err
			}
			if ok := controllerutil.RemoveFinalizer(unDyingProxy, finalizerName); !ok {
				// finalizer already removed
				return nil
			}
			err := r.Update(ctx, unDyingProxy)
			// ignore not found errors
			if err != nil && apierrors.IsNotFound(err) {
				return nil
			}
			return err
		})
		if err != nil {
			forwardingErrorsTotal.WithLabelValues("FailedRemoveFinalizer").Inc()
			gLog.Error(err, "Failed to remove finalizer")
			return ctrl.Result{}, err
		}

		// end reconciliation
		return ctrl.Result{}, nil
	}

	// Add the finalizer if it doesn't exist
	if unDyingProxy.GetDeletionTimestamp().IsZero() &&
		!controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
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
			forwardingErrorsTotal.WithLabelValues("FailedAddFinalizer").Inc()
			gLog.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}

	}

	// TCP
	if unDyingProxy.Spec.TCP != nil {
		// Setup the TCP Proxy if not already
		tcpForwardersMux.Lock()
		_, ok := tcpForwarders[unDyingProxy.Name]
		tcpForwardersMux.Unlock()
		if ok {
			gLog.V(1).Info("Forwarder already running")
		} else {
			forwarder, err := setupTCPForwarder(unDyingProxy)
			if err != nil {
				forwardingErrorsTotal.WithLabelValues("FailedSetupForwarder").Inc()
				gLog.Error(err, "Failed to setup TCP Forwarder")
				return ctrl.Result{}, err
			}
			gLog.V(1).Info("TCP Forwarder is now running")
			tcpForwardersMux.Lock()
			tcpForwarders[unDyingProxy.Name] = forwarder
			tcpForwardersMux.Unlock()
		}

		// Optionally manage the Kubernetes Service
		if r.TCPServiceToManage != "" {
			if err := r.manageServiceForUnDyingProxy(
				ctx,
				gLog,
				unDyingProxy.Name,
				r.TCPServiceToManage,
				unDyingProxy.Namespace,
				unDyingProxy.Spec.TCP.ListenPort,
				v1.ProtocolTCP,
			); err != nil {
				forwardingErrorsTotal.WithLabelValues("FailedManageService").Inc()
				gLog.Error(err, "Failed to manage Service for UnDyingProxy")
				return ctrl.Result{}, err
			}
		}
	}

	// UDP
	if unDyingProxy.Spec.UDP != nil {
		// Setup the UDP Proxy if not already
		udpForwardersMux.Lock()
		_, ok := udpForwarders[unDyingProxy.Name]
		udpForwardersMux.Unlock()
		if ok {
			gLog.V(1).Info("Forwarder already running")
		} else {
			forwarder, err := setupUDPForwarder(unDyingProxy)
			if err != nil {
				forwardingErrorsTotal.WithLabelValues("FailedSetupForwarder").Inc()
				gLog.Error(err, "Failed to setup UDP Forwarder")
				return ctrl.Result{}, err
			}
			// TODO log better between UDP and TCP
			gLog.V(1).Info("UDP Forwarder is now running")
			udpForwardersMux.Lock()
			udpForwarders[unDyingProxy.Name] = forwarder
			udpForwardersMux.Unlock()
		}

		// Optionally manage the Kubernetes Service
		if r.UDPServiceToManage != "" {
			if err := r.manageServiceForUnDyingProxy(
				ctx,
				gLog,
				unDyingProxy.Name,
				r.UDPServiceToManage,
				unDyingProxy.Namespace,
				unDyingProxy.Spec.UDP.ListenPort,
				v1.ProtocolUDP,
			); err != nil {
				forwardingErrorsTotal.WithLabelValues("FailedManageService").Inc()
				gLog.Error(err, "Failed to manage Service for UnDyingProxy")
				return ctrl.Result{}, err
			}
		}
	}

	// Set the ready status if it's not already
	if unDyingProxy.Status.Ready {
		return ctrl.Result{}, nil
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, req.NamespacedName, unDyingProxy); err != nil {
			return err
		}
		unDyingProxy.Status.Ready = true
		return r.Status().Update(ctx, unDyingProxy)
	})
	if err != nil {
		forwardingErrorsTotal.WithLabelValues("FailedUpdateStatus").Inc()
		gLog.Error(err, "Failed to update status")
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

// setupTCPForwarder sets up a TCP forwarder for an UnDyingProxy
// Run once per UnDyingProxy
func setupTCPForwarder(unDyingProxy *proxyv1alpha1.UnDyingProxy) (*TCPForwarder, error) {
	listenAddr := fmt.Sprintf(":%d", unDyingProxy.Spec.TCP.ListenPort)
	targetAddr := fmt.Sprintf("%s:%d", unDyingProxy.Spec.TCP.TargetHost, unDyingProxy.Spec.TCP.TargetPort)
	fLog := ctrl.Log.WithValues(
		"name", unDyingProxy.Name,
		"protocol", "TCP",
		"listen", listenAddr,
		"target", targetAddr,
	)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("Failed to setup listener: %w", err)
	}

	forwarder := &TCPForwarder{
		name:      unDyingProxy.Name,
		closeChan: make(chan struct{}),
		listener:  listener,
	}

	go func() {
		defer func() {
			listener.Close()
			fLog.V(1).Info("Listener Closed")
			tcpForwardersMux.Lock()
			delete(tcpForwarders, forwarder.name)
			tcpForwardersMux.Unlock()
			close(forwarder.closeChan)
			tcpForwardersRunningM.Dec()
		}()

		for {
			select {
			case <-forwarder.closeChan:
				fLog.V(1).Info("Shutting down listener")
				return
			default:
				// connToListener represents a new connection to the listener from a client
				fLog.V(1).Info("Waiting for connection")
				connToListener, err := listener.Accept()
				if err != nil {
					// ignore connection closed error
					if errors.Is(err, net.ErrClosed) {
						fLog.V(2).Info("ignoring connection closed error")
						continue
					}
					fLog.Error(err, "Failed to Accept on listener")
					continue
				}
				fLog.V(1).Info("Accepted Connection", "Remote", connToListener.RemoteAddr().String())
				go forwardTCPConnections(connToListener, targetAddr, forwarder.name, fLog)
			}
		}
	}()
	tcpForwardersRunningM.Inc()

	return forwarder, nil
}

func forwardTCPConnections(connToListener net.Conn, targetAddr, forwarderName string, fLog logr.Logger) {
	// connToTarget represents a connection to the target server
	connToTarget, err := net.Dial("tcp", targetAddr)
	if err != nil {
		fLog.Error(err, "Dial failed")
		return
	}
	defer connToTarget.Close()
	fLog.V(1).Info("Forwarding for",
		"listener", connToListener.LocalAddr(),
		"target", connToTarget.RemoteAddr(),
	)

	// fromClientToTarget
	go func() {
		defer connToListener.Close()
		numBytesCopied, err := io.Copy(connToTarget, connToListener)
		if err != nil {
			fLog.Error(err, "Failed to copy data from client to target")
		}
		tcpFromClientsToTargetBytesTotalM.WithLabelValues(forwarderName).Add(float64(numBytesCopied))
	}()

	// fromTargetToClient
	numBytesCopied, err := io.Copy(connToListener, connToTarget)
	if err != nil {
		fLog.Error(err, "Failed to copy data from target to client")
	}
	tcpFromTargetToClientsBytesTotalM.WithLabelValues(forwarderName).Add(float64(numBytesCopied))
}

// setupUDPForwarder sets up a UDP forwarder for an UnDyingProxy
// Run once per UnDyingProxy
func setupUDPForwarder(unDyingProxy *proxyv1alpha1.UnDyingProxy) (*UDPForwarder, error) {
	// TODO re-resolve DNS occasionally
	// Resolve listen and target addresses
	addr := fmt.Sprintf(":%d", unDyingProxy.Spec.UDP.ListenPort)
	listenAddress, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve listen address: %w", err)
	}
	addr = fmt.Sprintf("%s:%d", unDyingProxy.Spec.UDP.TargetHost, unDyingProxy.Spec.UDP.TargetPort)
	targetAddress, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve target address: %w", err)
	}

	// no need to re-initialize the listenerConnection
	listenerConnection, err := net.ListenUDP("udp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("Failed to listen on port %d: %w", unDyingProxy.Spec.UDP.ListenPort, err)
	}

	timeoutSeconds := unDyingProxy.Spec.UDP.TargetReadTimeoutSeconds
	// default to 30 seconds
	if timeoutSeconds == 0 {
		timeoutSeconds = 30
	}

	forwarder := &UDPForwarder{
		name:               unDyingProxy.Name,
		closeChan:          make(chan struct{}),
		listenAddress:      listenAddress,
		targetAddress:      targetAddress,
		listenerConnection: listenerConnection,
		targetReadTimeout:  time.Duration(timeoutSeconds) * time.Second,
		clients:            make(map[string]*UDPClient),
		clientsMux:         &sync.Mutex{},
	}

	go runUDPClientsForwarder(forwarder)
	udpForwardersRunningM.Inc()

	return forwarder, nil
}

// runUDPClientsForwarder handles forwarding packets from clients to the target.
// it sets up a goroutine for each client to forward packets from the target back to that client.
func runUDPClientsForwarder(forwarder *UDPForwarder) {
	fLog := ctrl.Log.WithValues(
		"name", forwarder.name,
		"protocol", "UDP",
		"direction", "fromClientToTarget",
		"listen", forwarder.listenAddress.String(),
		"target", forwarder.targetAddress.String(),
	)
	defer func() {
		// stop target-to-client goroutines
		forwarder.clientsMux.Lock()
		for _, client := range forwarder.clients {
			client.targetConnection.Close()
			client.closeChan <- struct{}{}
		}
		forwarder.clientsMux.Unlock()

		// self-cleanup
		forwarder.listenerConnection.Close()
		udpForwardersMux.Lock()
		delete(udpForwarders, forwarder.name)
		udpForwardersMux.Unlock()
		close(forwarder.closeChan)
		udpForwardersRunningM.Dec()
	}()

	data := make([]byte, forwarder.udpBufferBytes)

	// this loops runs once for every packet forwarded from a client to the target
	for {
		select {
		case <-forwarder.closeChan:
			return
		default:
			// block until a packet is received
			numBytesRead, clientAddr, readErr := forwarder.listenerConnection.ReadFromUDP(data)
			// 'Callers should always process the n > 0 bytes returned before considering the error err.'
			if numBytesRead > 0 {
				// track each client that connects to the listenAddress, reuse targetConnection if exists
				clientAddrStr := clientAddr.String()
				client, found := forwarder.clients[clientAddrStr]
				if !found {
					// setup the target connection
					targetConnection, err := net.DialUDP("udp", nil, forwarder.targetAddress)
					if err != nil {
						fLog.V(1).Error(err, "Error connecting to target")
						continue
					}

					// create and track this client
					client = &UDPClient{
						closeChan:        make(chan struct{}),
						clientAddress:    clientAddr,
						targetConnection: targetConnection,
					}
					forwarder.clientsMux.Lock()
					forwarder.clients[clientAddrStr] = client
					forwarder.clientsMux.Unlock()
					udpClientsConnectedM.WithLabelValues(forwarder.name).Inc()

					// handle responses from the target to this client
					go runUDPTargetForwarder(forwarder, client)
				}

				// write the packet to target
				// TODO set write deadline?
				numBytesWritten, err := client.targetConnection.Write(data[:numBytesRead])
				if err != nil {
					// continue sending more packets when the target is down
					fLog.V(1).Error(err, "Error writing to target")
					continue
				}

				udpFromClientsToTargetBytesTotalM.WithLabelValues(forwarder.name).Add(float64(numBytesWritten))

				err = client.targetConnection.SetReadDeadline(time.Now().Add(forwarder.targetReadTimeout))
				if err != nil {
					// continue sending more packets
					fLog.Error(err, "Error setting read deadline")
					continue
				}
			}

			if readErr != nil {
				// shutdown/remove when error reading from client, client will reconnect
				if clientAddr != nil { // check if this can ever be nil
					client, found := forwarder.clients[clientAddr.String()]
					if found {
						client.targetConnection.Close()
						client.closeChan <- struct{}{}
					}
				}

				// ignore if listener is closed
				if !errors.Is(readErr, net.ErrClosed) {
					fLog.V(1).Error(readErr, "Error reading from listener")
				}
			}
		}
	}
}

// runUDPTargetForwarder handles forwarding packets from the target back to a specific client
func runUDPTargetForwarder(forwarder *UDPForwarder, client *UDPClient) {
	fLog := ctrl.Log.WithValues(
		"name", forwarder.name,
		"direction", "fromTargetToClient",
		"target", forwarder.targetAddress,
		"client", client.clientAddress,
	)
	defer func() {
		// ending this function cleans up the client
		client.targetConnection.Close()
		forwarder.clientsMux.Lock()
		delete(forwarder.clients, client.clientAddress.String())
		forwarder.clientsMux.Unlock()
		close(client.closeChan)
		udpClientsConnectedM.WithLabelValues(forwarder.name).Dec()
	}()

	data := make([]byte, forwarder.udpBufferBytes)

	// this loops runs once for every packet returned to the client
	for {
		select {
		case <-client.closeChan:
			return
		default:
			// set read deadline so this goroutine will eventually shutdown
			err := client.targetConnection.SetReadDeadline(time.Now().Add(forwarder.targetReadTimeout))
			if err != nil {
				fLog.Error(err, "Error setting read deadline")
				return
			}

			// block until a packet is received or deadline is reached
			numBytesRead, _, readErr := client.targetConnection.ReadFromUDP(data)
			// 'Callers should always process the n > 0 bytes returned before considering the error err.'
			if numBytesRead > 0 {
				numBytesWritten, err := forwarder.listenerConnection.WriteToUDP(data[:numBytesRead], client.clientAddress)
				if err != nil {
					fLog.Error(err, "Error writing to client")
					return
				}

				udpFromTargetToClientsBytesTotalM.WithLabelValues(forwarder.name).Add(float64(numBytesWritten))
			}

			if readErr != nil {
				// ignore timeout error
				if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
					return
				}
				fLog.Error(readErr, "Error reading from target")
				return
			}
		}
	}
}

func (r *UnDyingProxyReconciler) manageServiceForUnDyingProxy(
	ctx context.Context,
	gLog logr.Logger,
	unDyingProxyName string,
	serviceName string,
	serviceNamespace string,
	listenPort int,
	protocol v1.Protocol,
) error {
	sLog := gLog.WithValues("Service.Name", serviceName)
	// first check if the Service is already to spec
	namespacedName := types.NamespacedName{Name: serviceName, Namespace: serviceNamespace}
	service := &v1.Service{}
	if err := r.Get(ctx, namespacedName, service); err != nil {
		return err
	}
	// check for a matching port spec
	for _, existingPort := range service.Spec.Ports {
		if existingPort.Name == unDyingProxyName &&
			existingPort.Port == int32(listenPort) &&
			// targetPort is listenPort because this app is the target
			existingPort.TargetPort == intstr.FromInt(listenPort) &&
			existingPort.Protocol == protocol {

			// already configured
			sLog.V(1).Info("UnDyingProxy Service already up to date")
			return nil
		}
	}

	// update the Service
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get Service
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		// add/update the port
		// check if exists, update if so
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
			// add new
			portSpec := v1.ServicePort{
				Name: unDyingProxyName,
				Port: int32(listenPort),
				// targetPort is listenPort because this app is the target
				TargetPort: intstr.FromInt(listenPort),
				Protocol:   protocol,
			}
			serviceCopy.Spec.Ports = append(serviceCopy.Spec.Ports, portSpec)
		}

		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("Failed to update (%s) Service to set Port: (%w)", service.Name, err)
	}

	sLog.V(1).Info("Updated Service with listenPort", "Port", listenPort)

	return nil
}

func (r *UnDyingProxyReconciler) cleanupServiceForUnDyingProxy(
	ctx context.Context,
	gLog logr.Logger,
	unDyingProxyName string,
	serviceName string,
	serviceNamespace string,
	listenPort int,
) error {
	service := &v1.Service{}
	namespacedName := types.NamespacedName{Name: serviceName, Namespace: serviceNamespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get Service
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		// remove the port
		for index, existingPort := range serviceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxyName {
				serviceCopy.Spec.Ports = append(
					serviceCopy.Spec.Ports[:index],
					serviceCopy.Spec.Ports[index+1:]...)
				break
			}
		}

		// patch
		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("Failed to update (%s) Service to remove Port: (%w)", service.Name, err)
	}

	gLog.V(1).Info("Updated Service to remove listenPort", "Port", listenPort)

	return nil
}
