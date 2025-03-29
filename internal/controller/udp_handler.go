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
	"net"
	"os"
	"sync"
	"time"

	"github.com/go-logr/logr"
	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
)

var (
	udpForwarders    = make(map[string]*udpForwarder)
	udpForwardersMux = &sync.Mutex{}
)

// udpForwarder tracks the UDP listener and connections for an UnDyingProxy
type udpForwarder struct {
	name               string
	closeChan          chan struct{}
	listenAddress      *net.UDPAddr
	targetAddress      *net.UDPAddr
	listenerConnection *net.UDPConn
	clients            map[string]*udpClient
	clientsMux         *sync.Mutex
	writeTimeout       time.Duration
	readTimeout        time.Duration
	udpBufferBytes     int
	log                logr.Logger
}

// udpClient tracks a client and target connection
type udpClient struct {
	// clientAddress is the IP address to the client, e.g. the game client
	clientAddress *net.UDPAddr
	// targetConnection is the connection to the target, e.g. the game server
	targetConnection *net.UDPConn
	log              logr.Logger
}

func (r *UnDyingProxyReconciler) handleUDPCleanup(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "UDP")

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

// handleUDP handles the UDP configuration for an UnDyingProxy
func (r *UnDyingProxyReconciler) handleUDP(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
) (bool, ctrl.Result, error) {
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues("protocol", "UDP")

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

// setupUDPForwarder sets up a UDP forwarder for an UnDyingProxy
// runs once per UnDyingProxy
func setupUDPForwarder(
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	udpBufferBytes int,
) (*udpForwarder, error) {
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

	forwarder := &udpForwarder{
		name:               unDyingProxy.Name,
		closeChan:          make(chan struct{}),
		listenAddress:      listenAddress,
		targetAddress:      targetAddress,
		listenerConnection: listenerConnection,
		writeTimeout:       time.Duration(writeTimeoutSeconds) * time.Second,
		readTimeout:        time.Duration(readTimeoutSeconds) * time.Second,
		clients:            make(map[string]*udpClient),
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
func forwardUDPClientsToTarget(forwarder *udpForwarder) {
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

func shutdownUDPClientsToTargetForwarder(forwarder *udpForwarder) {
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
	forwarder *udpForwarder,
	clientAddr *net.UDPAddr,
) (*udpClient, error) {
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

	udpClient := &udpClient{
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
func forwardUDPTargetToClient(forwarder *udpForwarder, udpClient *udpClient) {
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
	forwarder *udpForwarder,
	udpClient *udpClient,
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
