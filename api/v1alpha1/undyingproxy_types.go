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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// UDP defines a UDP port forwarder
type UDP struct {
	// ListenPort is the port to listen on
	ListenPort int `json:"listenPort"`
	// TargetPort is the port to forward to
	TargetPort int `json:"targetPort"`
	// TargetHost is the address to forward to, IP or DNS resolvable name
	TargetHost string `json:"targetHost"`
	// ReadTimeoutSeconds is the timeout for reading from the client and target. Defaults to 30 seconds.
	ReadTimeoutSeconds int `json:"readTimeoutSeconds,omitempty"`
	// WriteTimeoutSeconds is the timeout for writing to the client and target. Defaults to 5 seconds.
	WriteTimeoutSeconds int `json:"writeTimeoutSeconds,omitempty"`
}

// TCP defines a TCP port forwarder
type TCP struct {
	// ListenPort is the port to listen on
	ListenPort int `json:"listenPort"`
	// TargetPort is the port to forward to
	TargetPort int `json:"targetPort"`
	// TargetHost is the address to forward to, IP or DNS resolvable name
	TargetHost string `json:"targetHost"`
}

// UnDyingProxySpec defines the desired state of UnDyingProxy
type UnDyingProxySpec struct {
	// TODO make these immutable?, better yet, support mutability
	UDP *UDP `json:"udp,omitempty"`
	TCP *TCP `json:"tcp,omitempty"`
}

// UnDyingProxyStatus defines the observed state of UnDyingProxy
type UnDyingProxyStatus struct {
	// Ready is true when the proxy is ready to accept connections
	Ready bool `json:"ready"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="boolean",JSONPath=`.status.ready`
// +kubebuilder:printcolumn:name="ListenUDP",type="number",JSONPath=`.spec.udp.listenPort`
// +kubebuilder:printcolumn:name="ListenTCP",type="number",JSONPath=`.spec.tcp.listenPort`

// UnDyingProxy is the Schema for the undyingproxies API
type UnDyingProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   UnDyingProxySpec   `json:"spec,omitempty"`
	Status UnDyingProxyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// UnDyingProxyList contains a list of UnDyingProxy
type UnDyingProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []UnDyingProxy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&UnDyingProxy{}, &UnDyingProxyList{})
}
