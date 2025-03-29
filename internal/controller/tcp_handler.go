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

	"github.com/go-logr/logr"
	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"

	v1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	// tcpForwarders maps UnDyingProxy names to their active tcpForwarder instances.
	tcpForwarders = make(map[string]*tcpForwarder)
	// tcpForwardersMux protects concurrent access to the tcpForwarders map.
	tcpForwardersMux = &sync.Mutex{}
)

// tcpForwarder tracks the TCP listener for an UnDyingProxy
type tcpForwarder struct {
	name      string
	closeChan chan struct{}
	listener  net.Listener
	log       logr.Logger
}

// handleTCPCleanup stops the TCP forwarder associated with the given UnDyingProxy.
// It signals the forwarder to close, closes the listener, removes the forwarder
// from the global map, and triggers the cleanup of the associated Service port.
// It returns true if the ctrl.Result and error should be returned to the caller,
// false if the reconciliation should continue.
//
//nolint:unparam
func (r *UnDyingProxyReconciler) handleTCPCleanup(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "TCP")

	tcpForwardersMux.Lock()
	forwarder, ok := tcpForwarders[unDyingProxy.Name]
	if !ok {
		log.V(2).Info("TCP Forwarder was already stopped")
	} else {
		go func() {
			forwarder.closeChan <- struct{}{}
		}()
		err := forwarder.listener.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close TCP listener on forwarder shutdown")
		}
		log.V(2).Info("TCP Forwarder is now stopped")
	}
	tcpForwardersMux.Unlock()

	if r.TCPServiceToManage != "" {
		err := r.cleanupServiceForUnDyingProxy(ctx, unDyingProxy, v1.ProtocolTCP)
		if err != nil {
			log.Error(err, "Failed to cleanup Service for UnDyingProxy")
			mOperatorErrorsTotal.WithLabelValues("FailedServiceCleanup").Inc()
			return true, ctrl.Result{}, err
		}
		log.V(2).Info("Service for UnDyingProxy is now cleaned up")
	}

	return false, ctrl.Result{}, nil
}

// handleTCP ensures the TCP forwarder for the given UnDyingProxy is running.
// If a forwarder doesn't exist, it calls setupTCPForwarder to create and start one.
// It also ensures the associated Kubernetes Service port is correctly configured.
// It returns true if the ctrl.Result and error should be returned to the caller,
// false if the reconciliation should continue.
//
//nolint:unparam
func (r *UnDyingProxyReconciler) handleTCP(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "TCP")

	tcpForwardersMux.Lock()
	_, ok := tcpForwarders[unDyingProxy.Name]
	if ok {
		log.V(2).Info("Forwarder already running")
	} else {
		forwarder, err := setupTCPForwarder(unDyingProxy)
		if err != nil {
			mOperatorErrorsTotal.WithLabelValues("FailedSetupTCPForwarder").Inc()
			log.Error(err, "Failed to setup Forwarder")
			return true, ctrl.Result{}, err
		}
		log.V(2).Info("Forwarder is now running")
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

		log.V(2).Info("Service for UnDyingProxy is up to date")
	}

	return false, ctrl.Result{}, nil
}

// setupTCPForwarder creates and starts a new TCP forwarder for an UnDyingProxy.
// It resolves listen and target addresses, creates a TCP listener, initializes
// the tcpForwarder struct, starts the acceptTCPConnections goroutine, and increments
// the running forwarder metric.
// It runs once per UnDyingProxy with a TCP configuration.
func setupTCPForwarder(
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (*tcpForwarder, error) {
	listenAddr := fmt.Sprintf(":%d", unDyingProxy.Spec.TCP.ListenPort)
	targetAddr := fmt.Sprintf("%s:%d", unDyingProxy.Spec.TCP.TargetHost, unDyingProxy.Spec.TCP.TargetPort)

	log := ctrl.Log.WithName("tcpForwarder").WithValues(
		"name", unDyingProxy.Name,
		"protocol", "TCP",
		"listen", listenAddr,
		"target", targetAddr,
	)

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to setup listener: %w", err)
	}

	forwarder := &tcpForwarder{
		name:      unDyingProxy.Name,
		closeChan: make(chan struct{}),
		listener:  listener,
		log:       log,
	}

	go acceptTCPConnections(forwarder, listener, targetAddr)
	mTCPForwardersRunning.Inc()

	return forwarder, nil
}

// acceptTCPConnections runs in a dedicated goroutine for each TCP forwarder.
// It continuously accepts incoming client connections on the listener.
// For each accepted connection, it launches a forwardTCPConnection goroutine.
// It handles listener closure and errors during accept.
func acceptTCPConnections(
	forwarder *tcpForwarder,
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

// shutdownAcceptTCPConnections handles the cleanup when the acceptTCPConnections goroutine exits.
// It ensures the listener is closed, removes the forwarder from the global map,
// closes the forwarder's closeChan, and decrements the running forwarder metric.
func shutdownAcceptTCPConnections(
	forwarder *tcpForwarder,
	listener net.Listener,
) {
	log := forwarder.log
	log.V(2).Info("Shutting down TCP listener")

	tcpForwardersMux.Lock()

	err := listener.Close()
	if err != nil && !errors.Is(err, net.ErrClosed) {
		log.Error(err, "Failed to close listener")
	} else {
		log.V(2).Info("Listener Closed")
	}

	delete(tcpForwarders, forwarder.name)
	tcpForwardersMux.Unlock()

	close(forwarder.closeChan)
	mTCPForwardersRunning.Dec()
}

// forwardTCPConnection runs in a dedicated goroutine for each client TCP connection.
// It establishes a connection to the target server and then uses io.Copy in separate
// goroutines to proxy data bidirectionally between the client and the target.
// It handles connection closures and errors during data copying, updating relevant metrics.
func forwardTCPConnection(
	forwarder *tcpForwarder,
	connToListener net.Conn,
	targetAddr string,
) {
	log := forwarder.log.WithValues("clientAddr", connToListener.RemoteAddr().String(), "targetAddr", targetAddr)

	// connToTarget represents a connection to the target server
	connToTarget, err := net.Dial("tcp", targetAddr)
	if err != nil {
		mTCPForwardingErrorsTotal.WithLabelValues("DialServerTarget").Inc()
		log.Error(err, "Dial failed")
		return
	}
	defer func() {
		log.V(1).Info("Closing target connection")
		err = connToTarget.Close()
		if err != nil && !errors.Is(err, net.ErrClosed) {
			log.Error(err, "Failed to close target connection")
		}
	}()

	// fromClientToTarget
	go func() {
		defer func() {
			log.V(2).Info("Closing client listener connection")
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
