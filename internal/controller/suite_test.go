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
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var cfg *rest.Config
var k8sClient client.Client
var testEnv *envtest.Environment

const tcpSvcToManage = "undying-proxy-tcp"
const udpSvcToManage = "undying-proxy-udp"
const namespace = "default"

var tcpSvcTypeNamespacedName = types.NamespacedName{
	Name:      tcpSvcToManage,
	Namespace: namespace,
}
var udpSvcTypeNamespacedName = types.NamespacedName{
	Name:      udpSvcToManage,
	Namespace: namespace,
}

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,

		// The BinaryAssetsDirectory is only required if you want to run the tests directly
		// without call the makefile target test. If not informed it will look for the
		// default path defined in controller-runtime which is /usr/local/kubebuilder/.
		// Note that you must have the required binaries setup under the bin directory to perform
		// the tests directly. When we run make test it will be setup and used automatically.
		BinaryAssetsDirectory: filepath.Join("..", "..", "bin", "k8s",
			fmt.Sprintf("1.30.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = proxyv1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	// +kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Create the Services to manage
	ctx := context.Background()
	service := &v1.Service{}

	By("creating the TCP Service to manage")
	err = k8sClient.Get(ctx, tcpSvcTypeNamespacedName, service)
	if err != nil && errors.IsNotFound(err) {
		resource := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      tcpSvcToManage,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Ports: []v1.ServicePort{
					{
						// only here to pass validation
						Name:       "any-port",
						Port:       5000,
						TargetPort: intstr.FromInt(5000),
						Protocol:   v1.ProtocolTCP,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	}

	By("creating the UDP Service to manage")
	err = k8sClient.Get(ctx, udpSvcTypeNamespacedName, service)
	if err != nil && errors.IsNotFound(err) {
		resource := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      udpSvcToManage,
				Namespace: namespace,
			},
			Spec: v1.ServiceSpec{
				Ports: []v1.ServicePort{
					{
						// only here to pass validation
						Name:       "any-port",
						Port:       5000,
						TargetPort: intstr.FromInt(5000),
						Protocol:   v1.ProtocolUDP,
					},
				},
			},
		}
		Expect(k8sClient.Create(ctx, resource)).To(Succeed())
	}
})

var _ = AfterSuite(func() {
	ctx := context.Background()

	// TCP svc
	tcpSvcResource := &v1.Service{}
	err := k8sClient.Get(ctx, tcpSvcTypeNamespacedName, tcpSvcResource)
	Expect(err).NotTo(HaveOccurred())

	By("Cleanup the TCP Service to manage")
	Expect(k8sClient.Delete(ctx, tcpSvcResource)).To(Succeed())

	// UDP svc
	udpSvcResource := &v1.Service{}
	err = k8sClient.Get(ctx, udpSvcTypeNamespacedName, udpSvcResource)
	Expect(err).NotTo(HaveOccurred())

	By("Cleanup the UDP Service to manage")
	Expect(k8sClient.Delete(ctx, udpSvcResource)).To(Succeed())

	By("tearing down the test environment")
	err = testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})
