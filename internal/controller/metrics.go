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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
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
		mUDPForwardingErrorsTotal,
		mOperatorErrorsTotal,
	)
}
