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
	tcpForwarders    = make(map[string]*tcpForwarder)
	tcpForwardersMux = &sync.Mutex{}
)

// tcpForwarder tracks the TCP listener for an UnDyingProxy
type tcpForwarder struct {
	name      string
	closeChan chan struct{}
	listener  net.Listener
	log       logr.Logger
}

// handleTCPCleanup stops the TCP forwarder and cleans up related resources
func (r *UnDyingProxyReconciler) handleTCPCleanup(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "TCP")

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

// handleTCP handles the TCP configuration for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleTCP(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "TCP")

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

// setupTCPForwarder sets up a TCP forwarder for an UnDyingProxy
// runs once per UnDyingProxy
func setupTCPForwarder(unDyingProxy *proxyv1alpha1.UnDyingProxy) (*tcpForwarder, error) {
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

// acceptTCPConnections listens for new TCP connections and
// sets up goroutines to forward them.
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

func shutdownAcceptTCPConnections(
	forwarder *tcpForwarder,
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
	forwarder *tcpForwarder,
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
