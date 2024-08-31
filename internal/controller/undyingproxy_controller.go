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
	"flag"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/go-logr/logr"
	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

const (
	finalizerName = "undyingproxies.proxy.sfact.io/finalizer"
)

var (
	// forwarders is a map of Forwarders to track globally in memory
	forwarders = make(map[string]*Forwarder)
	// forwardersMux is a mutex lock for modifying forwarders
	forwardersMux = &sync.Mutex{}
	patchOptions  = &client.PatchOptions{FieldManager: "undying-proxy-operator"}
)

// configuration vars
var (
	svcToManage string
)

// UnDyingProxyReconciler reconciles a UnDyingProxy object
type UnDyingProxyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// Forwarder tracks the listener and connections for an UnDyingProxy
type Forwarder struct {
	name string
	// closeChan is for closing the targetForwarder goroutine, which will in turn close the client connections
	closeChan          chan struct{}
	listenAddress      *net.UDPAddr
	targetAddress      *net.UDPAddr
	listenerConnection *net.UDPConn
	clients            map[string]*Client
	targetReadTimeout  time.Duration
}

// Client tracks a client and target connection
type Client struct {
	// closeChan is for closing the targetForwarder goroutine
	closeChan chan struct{}

	// clientConnection is the connection to the client, e.g. the game client
	clientAddress *net.UDPAddr
	// targetConnection is the connection to the target, e.g. the game server
	targetConnection *net.UDPConn
}

func init() {
	// TODO convert this to metrics
	// go func() {
	//     for {
	//         time.Sleep(5 * time.Second)
	//         fmt.Println("Running Forwarders:")
	//         for name, forwarder := range forwarders {
	//             fmt.Printf("%s: %+v\n", name, forwarder)
	//         }
	//     }
	// }()
	flag.StringVar(
		&svcToManage,
		"service-to-manage",
		"undying-proxy",
		"A Kubernetes Service Object to manage, adding/removing listenPorts, in order to expose this app to traffic for the UnDyingProxies. Service must preexist. Set to blank to disable this feature and manage the Service yourself.",
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
		gLog.Error(err, "Failed to fetch UnDyingProxy")

		return ctrl.Result{}, err
	}

	// Handle the UnDyingProxy being deleted
	if !unDyingProxy.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle the rare case where it's being deleted but the finalizer is not present
		if !controllerutil.ContainsFinalizer(unDyingProxy, finalizerName) {
			return ctrl.Result{}, nil
		}

		// Confirm that the UnDyingProxy still exists
		err := r.Get(ctx, req.NamespacedName, unDyingProxy)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, err
		}

		gLog.V(1).Info("UnDyingProxy is being deleted, performing Finalizer Operations")

		// ensure shutdown of the forwarder
		if forwarder, ok := forwarders[unDyingProxy.Name]; !ok {
			gLog.V(1).Info("Forwarder is already closed")
		} else {
			forwarder.listenerConnection.Close()
			forwarder.closeChan <- struct{}{}
			close(forwarder.closeChan)
			forwardersMux.Lock()
			delete(forwarders, forwarder.name)
			forwardersMux.Unlock()
			gLog.V(1).Info("Forwarder is now closed")
		}

		// Optionally manage the Kubernetes Service
		if svcToManage != "" {
			if err := r.cleanupServiceForUnDyingProxy(ctx, svcToManage, unDyingProxy, gLog); err != nil {
				gLog.Error(err, "Failed to cleanup Service for UnDyingProxy")
				return ctrl.Result{}, err
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
			if controllerutil.AddFinalizer(unDyingProxy, finalizerName) {
				return r.Update(ctx, unDyingProxy)
			}
			return nil
		})
		if err != nil {
			gLog.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}

	}

	// Setup the UDP Proxy if not already
	if _, ok := forwarders[unDyingProxy.Name]; ok {
		gLog.V(1).Info("Forwarder already running")
	} else {
		forwarder, err := setupUDPForwarder(unDyingProxy)
		if err != nil {
			gLog.Error(err, "Failed to setup UDP Forwarder")
			return ctrl.Result{}, err
		}
		gLog.V(1).Info("Forwarder is now running")
		forwardersMux.Lock()
		forwarders[unDyingProxy.Name] = forwarder
		forwardersMux.Unlock()
	}

	// Optionally manage the Kubernetes Service
	if svcToManage != "" {
		if err := r.manageServiceForUnDyingProxy(ctx, svcToManage, unDyingProxy, gLog); err != nil {
			gLog.Error(err, "Failed to manage Service for UnDyingProxy")
			return ctrl.Result{}, err
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
		gLog.Error(err, "Failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *UnDyingProxyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&proxyv1alpha1.UnDyingProxy{}).
		Complete(r)
}

// setupUDPForwarder sets up a UDP forwarder for an UnDyingProxy
// Run once per UnDyingProxy
func setupUDPForwarder(unDyingProxy *proxyv1alpha1.UnDyingProxy) (*Forwarder, error) {
	// Resolve listen and target addresses
	addr := fmt.Sprintf(":%d", unDyingProxy.Spec.ListenPort)
	listenAddress, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve listen address: %w", err)
	}
	addr = fmt.Sprintf("%s:%d", unDyingProxy.Spec.TargetHost, unDyingProxy.Spec.TargetPort)
	targetAddress, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("Failed to resolve target address: %w", err)
	}

	// AFAIK we never need to re-initialize the listenerConnection
	listenerConnection, err := net.ListenUDP("udp", listenAddress)
	if err != nil {
		return nil, fmt.Errorf("Failed to listen on port %d: %w", unDyingProxy.Spec.ListenPort, err)
	}

	timeoutSeconds := unDyingProxy.Spec.TargetReadTimeoutSeconds
	// default to 30 seconds
	if timeoutSeconds == 0 {
		timeoutSeconds = 30
	}
	forwarder := &Forwarder{
		name:               unDyingProxy.Name,
		closeChan:          make(chan struct{}),
		listenAddress:      listenAddress,
		targetAddress:      targetAddress,
		listenerConnection: listenerConnection,
		targetReadTimeout:  time.Duration(timeoutSeconds) * time.Second,
		clients:            make(map[string]*Client),
	}

	go runClientsForwarder(forwarder)

	return forwarder, nil
}

// runClientsForwarder handles forwarding packets from clients to the target.
// it sets up a goroutine for each client to forward packets from the target back to that client.
func runClientsForwarder(forwarder *Forwarder) {
	fLog := ctrl.Log.WithValues(
		"name", forwarder.name,
		"direction", "fromClientToTarget",
		"listen", forwarder.listenAddress.String(),
		"target", forwarder.targetAddress.String(),
	)
	clientsMux := &sync.Mutex{}
	// this loops runs once for every packet forwarded from the client
	for {
		select {
		case <-forwarder.closeChan:
			// close the targetForwarders
			for _, client := range forwarder.clients {
				// could just close the targetConnection here, which is better?
				client.closeChan <- struct{}{}
			}

			// TODO self-cleanup? or cleanup in the finalizer logic?
			forwardersMux.Lock()
			delete(forwarders, forwarder.name)
			forwardersMux.Unlock()
			forwarder.listenerConnection.Close()

			return
		default:
			buf := make([]byte, 1024)                                           // TODO configurable buffer size
			n, clientAddr, err := forwarder.listenerConnection.ReadFromUDP(buf) // blocks until a packet is received
			if err != nil {
				// ignore closed connection error, which happens when the listener is closed
				if !errors.Is(err, net.ErrClosed) {
					fLog.V(1).Error(err, "Error reading from listener")
				}
				continue
			}

			// we track each client that connects to the listenAddress, reuse targetConnection if exists
			clientAddrStr := clientAddr.String()
			client, ok := forwarder.clients[clientAddrStr]
			if ok {
				// client exists
				// write the packet to target
				_, err = client.targetConnection.Write(buf[:n])
				if err != nil {
					fLog.V(1).Error(err, "Error writing to target")
					continue
				}
				client.targetConnection.SetReadDeadline(time.Now().Add(forwarder.targetReadTimeout))

				// TODO: metrics instead
				// too expensive, can't log for every packet except in manual testing
				// fLog.V(2).Info(fmt.Sprintf("Forwarded %d bytes from %s to %s", n, clientAddrStr, forwarder.targetAddress.String()))

				continue
			}

			// client doesn't exist
			// create connection and write to target
			targetConnection, err := net.DialUDP("udp", nil, forwarder.targetAddress)
			if err != nil {
				fLog.V(1).Error(err, "Error connecting to target")
				continue
			}
			// n, err := targetConnection.Write(buf[:n])
			_, err = targetConnection.Write(buf[:n])
			if err != nil {
				fLog.V(1).Error(err, "Error writing to target")
				continue
			}

			// create and record the tracked Client
			client = &Client{
				closeChan:        make(chan struct{}),
				clientAddress:    clientAddr,
				targetConnection: targetConnection,
			}
			clientsMux.Lock()
			forwarder.clients[clientAddrStr] = client
			clientsMux.Unlock()

			// handle responses from the target to the client
			go runTargetForwarder(forwarder, client, clientsMux)
		}
	}
}

// runTargetForwarder handles forwarding packets from the target back to a specific client
func runTargetForwarder(forwarder *Forwarder, client *Client, clientsMux *sync.Mutex) {
	data := make([]byte, 1024) // TODO configurable buffer size
	fLog := ctrl.Log.WithValues(
		"name", forwarder.name,
		"direction", "fromTargetToClient",
		"target", forwarder.targetAddress,
		"client", client.clientAddress,
	)
ForwardingLoop:
	// this loops runs once for every packet returned to the client
	for {
		select {
		case <-client.closeChan:
			break ForwardingLoop
		default:
			client.targetConnection.SetReadDeadline(time.Now().Add(forwarder.targetReadTimeout))
			n, _, err := client.targetConnection.ReadFromUDP(data) // blocks until a packet is received or deadline is reached
			if err != nil {
				// ignore timeout error
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					continue ForwardingLoop
				}
				fLog.Error(err, "Error reading from target")
				break ForwardingLoop
			}

			_, err = forwarder.listenerConnection.WriteToUDP(data[:n], client.clientAddress)
			if err != nil {
				fLog.Error(err, "Error writing to client")
				break ForwardingLoop
			}

			// too expensive, can't log for every packet except in manual testing
			// fLog.V(2).Info(fmt.Sprintf("Forwarded %d bytes from %s to %s", n, forwarder.targetAddress, client.clientAddress))
		}
	}

	clientsMux.Lock()
	delete(forwarder.clients, client.clientAddress.String())
	clientsMux.Unlock()
	client.targetConnection.Close()
}

func (r *UnDyingProxyReconciler) manageServiceForUnDyingProxy(
	ctx context.Context,
	serviceToManage string,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	gLog logr.Logger,
) error {
	// first check if the Service is already to spec
	namespacedName := types.NamespacedName{Name: serviceToManage, Namespace: unDyingProxy.Namespace}
	udpService := &v1.Service{}
	if err := r.Get(ctx, namespacedName, udpService); err != nil {
		return err
	}
	// check for a matching port spec
	for _, existingPort := range udpService.Spec.Ports {
		if existingPort.Name == unDyingProxy.Name &&
			existingPort.Port == int32(unDyingProxy.Spec.ListenPort) &&
			// targetPort is listenPort because this app is the target
			existingPort.TargetPort == intstr.FromInt(unDyingProxy.Spec.ListenPort) &&
			existingPort.Protocol == v1.ProtocolUDP {

			// already configured
			gLog.V(1).Info("UnDyingProxy Service already up to date")
			return nil
		}
	}

	// update the Service
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get Service
		if err := r.Get(ctx, namespacedName, udpService); err != nil {
			return err
		}
		udpServiceCopy := udpService.DeepCopy()

		// add/update the port
		// check if exists, update if so
		exists := false
		for index, existingPort := range udpServiceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxy.Name {
				exists = true
				udpServiceCopy.Spec.Ports[index].Port = int32(unDyingProxy.Spec.ListenPort)
				udpServiceCopy.Spec.Ports[index].TargetPort = intstr.FromInt(unDyingProxy.Spec.ListenPort)
				udpServiceCopy.Spec.Ports[index].Protocol = v1.ProtocolUDP
				break
			}
		}

		if !exists {
			// add new
			portSpec := v1.ServicePort{
				Name: unDyingProxy.Name,
				Port: int32(unDyingProxy.Spec.ListenPort),
				// targetPort is listenPort because this app is the target
				TargetPort: intstr.FromInt(unDyingProxy.Spec.ListenPort),
				Protocol:   v1.ProtocolUDP,
			}
			udpServiceCopy.Spec.Ports = append(udpServiceCopy.Spec.Ports, portSpec)
		}

		return r.Patch(ctx, udpServiceCopy, client.MergeFrom(udpService), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("Failed to update (%s) Service to set Port: (%w)", udpService.Name, err)
	}

	gLog.V(1).Info(
		"Updated Service with listenPort",
		"Port",
		unDyingProxy.Spec.ListenPort,
	)

	return nil
}

func (r *UnDyingProxyReconciler) cleanupServiceForUnDyingProxy(
	ctx context.Context,
	serviceToManage string,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	gLog logr.Logger,
) error {
	udpService := &v1.Service{}
	namespacedName := types.NamespacedName{Name: serviceToManage, Namespace: unDyingProxy.Namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// get Service
		if err := r.Get(ctx, namespacedName, udpService); err != nil {
			return err
		}
		udpServiceCopy := udpService.DeepCopy()

		// remove the port
		for index, existingPort := range udpServiceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxy.Name {
				udpServiceCopy.Spec.Ports = append(
					udpServiceCopy.Spec.Ports[:index],
					udpServiceCopy.Spec.Ports[index+1:]...)
				break
			}
		}

		// patch
		return r.Patch(ctx, udpServiceCopy, client.MergeFrom(udpService), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("Failed to update (%s) Service to remove Port: (%w)", udpService.Name, err)
	}

	gLog.V(1).Info(
		"Updated Service to remove listenPort",
		"Port",
		unDyingProxy.Spec.ListenPort,
	)

	return nil
}
