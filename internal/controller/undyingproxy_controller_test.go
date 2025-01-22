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
	"runtime"
	"strconv"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

var _ = Describe("UnDyingProxy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const namespace = "default"

		ctx := context.Background()

		proxyNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespace,
		}

		controllerReconciler := &UnDyingProxyReconciler{}

		resourceToCreate := &proxyv1alpha1.UnDyingProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      resourceName,
				Namespace: namespace,
			},
			Spec: proxyv1alpha1.UnDyingProxySpec{
				UDP: &proxyv1alpha1.UDP{
					ListenPort:          3001,
					TargetPort:          3002,
					TargetHost:          "localhost",
					ReadTimeoutSeconds:  5,
					WriteTimeoutSeconds: 2,
				},
				TCP: &proxyv1alpha1.TCP{
					ListenPort: 5001,
					TargetPort: 5002,
					TargetHost: "localhost",
				},
			},
		}

		BeforeEach(func() {
			By("creating the reconciler")
			controllerReconciler = &UnDyingProxyReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				TCPServiceToManage: "undying-proxy-tcp",
				UDPServiceToManage: "undying-proxy-udp",
				UDPBufferBytes:     1024,
			}
		})

		It("should successfully reconcile the resource", func() {
			By("Creating the UnDyingProxy object")
			Expect(k8sClient.Create(ctx, resourceToCreate.DeepCopy())).To(Succeed())

			By("Reconciling the created resource")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the UnDyingProxy object is ready")
			resource := &proxyv1alpha1.UnDyingProxy{}
			err = k8sClient.Get(ctx, proxyNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.Ready).To(BeTrue())

			By("Confirming the UDP Service is managed")
			udpSvcResource := &v1.Service{}
			err = k8sClient.Get(ctx, udpSvcTypeNamespacedName, udpSvcResource)
			Expect(err).NotTo(HaveOccurred())
			Expect(udpSvcResource.Spec.Ports).To(ContainElement(v1.ServicePort{
				Name:       resourceName,
				Port:       3001,
				TargetPort: intstr.FromInt(3001),
				Protocol:   v1.ProtocolUDP,
			}))

			By("Confirming the TCP Service is managed")
			tcpSvcResource := &v1.Service{}
			err = k8sClient.Get(ctx, tcpSvcTypeNamespacedName, tcpSvcResource)
			Expect(err).NotTo(HaveOccurred())
			Expect(tcpSvcResource.Spec.Ports).To(ContainElement(v1.ServicePort{
				Name:       resourceName,
				Port:       5001,
				TargetPort: intstr.FromInt(5001),
				Protocol:   v1.ProtocolTCP,
			}))

			By("Cleanup the UnDyingProxy object")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())

			By("Reconciling the deletion")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should successfully cleanup, and re-create", func() {
			By("Creating the UnDyingProxy object")
			Expect(k8sClient.Create(ctx, resourceToCreate.DeepCopy())).To(Succeed())

			By("Reconciling the first time")
			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the number of goroutines and forwarders the first time")
			Expect(runtime.NumGoroutine()).To(BeNumerically("==", 11))
			Expect(tcpForwarders).To(HaveLen(1))
			Expect(udpForwarders).To(HaveLen(1))

			By("Sending traffic through the UDP tunnel the first time")
			err = sendTestDataOverUDP(resourceToCreate)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the UnDyingProxy object")
			unDyingProxy := &proxyv1alpha1.UnDyingProxy{}
			err = k8sClient.Get(ctx, proxyNamespacedName, unDyingProxy)
			Expect(err).NotTo(HaveOccurred())
			Expect(k8sClient.Delete(ctx, unDyingProxy)).To(Succeed())

			By("Reconciling the first delete")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the number of goroutines and forwarders after first delete")
			Expect(runtime.NumGoroutine()).To(BeNumerically("==", 9))
			Expect(tcpForwarders).To(BeEmpty())
			Expect(udpForwarders).To(BeEmpty())

			By("Confirming data can't be sent through after first delete")
			err = sendTestDataOverUDP(resourceToCreate)
			Expect(err).To(HaveOccurred())

			By("Recreating the UnDyingProxy object")
			Expect(k8sClient.Create(ctx, resourceToCreate.DeepCopy())).To(Succeed())

			By("Reconciling the UnDyingProxy object the second time")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the number of goroutines and forwarders the second time")
			Expect(runtime.NumGoroutine()).To(BeNumerically("==", 11))
			Expect(tcpForwarders).To(HaveLen(1))
			Expect(udpForwarders).To(HaveLen(1))

			By("Sending traffic through the UDP tunnel the second time")
			err = sendTestDataOverUDP(resourceToCreate)
			Expect(err).NotTo(HaveOccurred())

			By("Deleting the UnDyingProxy object the second time")
			Expect(k8sClient.Delete(ctx, unDyingProxy)).To(Succeed())

			By("Reconciling the second delete")
			_, err = controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: proxyNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			By("Confirming the number of goroutines and forwarders after second delete")
			Expect(runtime.NumGoroutine()).To(BeNumerically("==", 9))
			Expect(tcpForwarders).To(BeEmpty())
			Expect(udpForwarders).To(BeEmpty())
		})
	})
})

func sendTestDataOverUDP(undyingproxy *proxyv1alpha1.UnDyingProxy) error {
	// setup server
	targetAddressStr := undyingproxy.Spec.UDP.TargetHost + ":" + strconv.Itoa(undyingproxy.Spec.UDP.TargetPort)
	targetAddress, err := net.ResolveUDPAddr("udp", targetAddressStr)
	if err != nil {
		return err
	}

	serverConnection, err := net.ListenUDP("udp", targetAddress)
	if err != nil {
		return err
	}
	defer func() { _ = serverConnection.Close() }()

	go func() {
		buffer := make([]byte, 1024)
		for {
			n, clientAddress, err := serverConnection.ReadFromUDP(buffer)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					fmt.Println("Server connection closed")
					return
				}
				fmt.Println("Error with ReadFromUDP", err)
				continue
			}
			// Echo the data back to the client
			fmt.Printf("Received from %s: %s\n", clientAddress, string(buffer[:n]))
			_, err = serverConnection.WriteToUDP(buffer[:n], clientAddress)
			if err != nil {
				fmt.Println("Error writing to UDP:", err)
			}
		}
	}()

	// send message
	remoteAddressStr := "localhost:" + strconv.Itoa(undyingproxy.Spec.UDP.ListenPort)
	remoteAddr, err := net.ResolveUDPAddr("udp", remoteAddressStr)
	if err != nil {
		return err
	}

	clientConnection, err := net.DialUDP("udp", nil, remoteAddr)
	if err != nil {
		return err
	}
	defer clientConnection.Close()

	message := "Hello, UDP!"
	_, err = clientConnection.Write([]byte(message))
	if err != nil {
		return err
	}

	buffer := make([]byte, 1024)
	err = clientConnection.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err != nil {
		return err
	}
	n, _, err := clientConnection.ReadFromUDP(buffer)
	if err != nil {
		return err
	}

	returnedMessage := string(buffer[:n])
	fmt.Println("Received from server:", returnedMessage)

	if returnedMessage != message {
		return fmt.Errorf("Sent and received messages do not match")
	}

	fmt.Println("Sent and received messages match")

	_ = serverConnection.Close()

	return nil
}
