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

package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
	"github.com/snarlysodboxer/undying-proxy/internal/controller"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(proxyv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var operatorNamespace string
	var metricsAddr string
	var probeAddr string
	var authedMetrics bool
	var secureMetrics bool
	var enableHTTP2 bool
	var tcpSvcToManage string
	var udpSvcToManage string
	var udpBufferBytes int
	flag.StringVar(&operatorNamespace, "operator-namespace", "", "The Kubernetes Namespace in which to watch for UnDyingProxy objects.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&authedMetrics, "metrics-authed", true,
		"If true, the metrics endpoint is behind auth and requires a ClusterRole. See WithAuthenticationAndAuthorization function in the Controller-Runtime filters package.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	flag.StringVar(&tcpSvcToManage, "tcp-service-to-manage", "undying-proxy-tcp", "A Kubernetes Service Object to manage for TCP, adding/removing listenPorts, in order to expose this app to traffic for the UnDyingProxies. Service must preexist. Set to blank to disable this feature and manage the Service yourself.")
	flag.StringVar(&udpSvcToManage, "udp-service-to-manage", "undying-proxy-udp", "A Kubernetes Service Object to manage for UDP, adding/removing listenPorts, in order to expose this app to traffic for the UnDyingProxies. Service must preexist. Set to blank to disable this feature and manage the Service yourself.")
	flag.IntVar(&udpBufferBytes, "udp-buffer-bytes", 1024, "The size of the buffer for UDP packets.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if operatorNamespace == "" {
		setupLog.Error(fmt.Errorf("operator-namespace must be set"), "operator-namespace must be set")
		os.Exit(1)
	}

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	tlsOpts := []func(*tls.Config){}
	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookServer := webhook.NewServer(webhook.Options{
		TLSOpts: tlsOpts,
	})

	filter := filters.WithAuthenticationAndAuthorization
	if !authedMetrics {
		filter = nil
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// Restrict to a single Namespace
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				operatorNamespace: {},
			},
		},
		// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
		// More info:
		// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/metrics/server
		// - https://book.kubebuilder.io/reference/metrics.html
		Metrics: metricsserver.Options{
			BindAddress:   metricsAddr,
			SecureServing: secureMetrics,
			// TLSOpts is used to allow configuring the TLS config used for the server. If certificates are
			// not provided, self-signed certificates will be generated by default. This option is not recommended for
			// production environments as self-signed certificates do not offer the same level of trust and security
			// as certificates issued by a trusted Certificate Authority (CA). The primary risk is potentially allowing
			// unauthorized access to sensitive metrics data. Consider replacing with CertDir, CertName, and KeyName
			// to provide certificates, ensuring the server communicates using trusted and secure certificates.
			TLSOpts: tlsOpts,
			// FilterProvider is used to protect the metrics endpoint with authn/authz.
			// These configurations ensure that only authorized users and service accounts
			// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
			// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.18.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
			FilterProvider: filter,
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.UnDyingProxyReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		UDPServiceToManage: udpSvcToManage,
		TCPServiceToManage: tcpSvcToManage,
		UDPBufferBytes:     udpBufferBytes,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "UnDyingProxy")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	// TODO add readiness check for having loaded all the UnDyingProxies
	//     perhaps count the number of UnDyingProxies at init time, then increment a counter for each processed UnDyingProxy, then readyz compares the two numbers?
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
