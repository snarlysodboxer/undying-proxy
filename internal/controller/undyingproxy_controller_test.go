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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
)

var _ = Describe("UnDyingProxy Controller", func() {
	Context("When reconciling a resource", func() {
		const resourceName = "test-resource"
		const namespace = "default"

		ctx := context.Background()

		udpTypeNamespacedName := types.NamespacedName{
			Name:      resourceName,
			Namespace: namespace,
		}
		undyingproxy := &proxyv1alpha1.UnDyingProxy{}

		BeforeEach(func() {
			By("creating the custom resource for the Kind UnDyingProxy")
			err := k8sClient.Get(ctx, udpTypeNamespacedName, undyingproxy)
			if err != nil && errors.IsNotFound(err) {
				resource := &proxyv1alpha1.UnDyingProxy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      resourceName,
						Namespace: namespace,
					},
					Spec: proxyv1alpha1.UnDyingProxySpec{
						UDP: &proxyv1alpha1.UDP{
							ListenPort: 3001,
							TargetPort: 3002,
							TargetHost: "localhost",
						},
						TCP: &proxyv1alpha1.TCP{
							ListenPort: 3001,
							TargetPort: 3002,
							TargetHost: "localhost",
						},
					},
				}
				Expect(k8sClient.Create(ctx, resource)).To(Succeed())
			}
		})

		AfterEach(func() {
			resource := &proxyv1alpha1.UnDyingProxy{}
			err := k8sClient.Get(ctx, udpTypeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())

			By("Cleanup the specific resource instance UnDyingProxy")
			Expect(k8sClient.Delete(ctx, resource)).To(Succeed())
		})
		It("should successfully reconcile the resource", func() {
			By("Reconciling the created resource")
			controllerReconciler := &UnDyingProxyReconciler{
				Client:             k8sClient,
				Scheme:             k8sClient.Scheme(),
				TCPServiceToManage: "undying-proxy-tcp",
				UDPServiceToManage: "undying-proxy-udp",
				UDPBufferBytes:     1024,
			}

			_, err := controllerReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: udpTypeNamespacedName,
			})
			Expect(err).NotTo(HaveOccurred())

			// status.Ready
			resource := &proxyv1alpha1.UnDyingProxy{}
			err = k8sClient.Get(ctx, udpTypeNamespacedName, resource)
			Expect(err).NotTo(HaveOccurred())
			Expect(resource.Status.Ready).To(BeTrue())

			// service is managed
			svcResource := &v1.Service{}
			err = k8sClient.Get(ctx, udpSvcTypeNamespacedName, svcResource)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcResource.Spec.Ports).To(ContainElement(v1.ServicePort{
				Name:       resourceName,
				Port:       3001,
				TargetPort: intstr.FromInt(3001),
				Protocol:   v1.ProtocolUDP,
			}))

			// TODO test sending UDP traffic through the tunnel
		})
	})
})
