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

	"github.com/go-logr/logr"
	proxyv1alpha1 "github.com/snarlysodboxer/undying-proxy/api/v1alpha1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *UnDyingProxyReconciler) manageServiceForUnDyingProxy(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	protocol v1.Protocol,
) error {
	protocolStr := "TCP"
	serviceName := r.TCPServiceToManage
	listenPort := unDyingProxy.Spec.TCP.ListenPort
	if protocol == v1.ProtocolUDP {
		protocolStr = "UDP"
		serviceName = r.UDPServiceToManage
		listenPort = unDyingProxy.Spec.UDP.ListenPort
	}
	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues(
		"protocol", protocolStr,
		"Service.Name", serviceName,
		"listenPort", listenPort,
	)

	namespacedName := types.NamespacedName{Name: serviceName, Namespace: unDyingProxy.Namespace}
	service := &v1.Service{}
	if err := r.Get(ctx, namespacedName, service); err != nil {
		return err
	}

	for _, existingPort := range service.Spec.Ports {
		if existingPort.Name == unDyingProxy.Name &&
			existingPort.Port == int32(listenPort) &&
			// targetPort is listenPort because this app is the target
			existingPort.TargetPort == intstr.FromInt(listenPort) &&
			existingPort.Protocol == protocol {
			log.V(1).Info("Service already up to date")

			return nil
		}
	}

	err := r.updateService(
		ctx,
		namespacedName,
		service,
		listenPort,
		protocol,
		unDyingProxy.Name,
	)
	if err != nil {
		return err
	}

	log.V(1).Info("Updated Service with listenPort")

	return nil
}

func (r *UnDyingProxyReconciler) updateService(
	ctx context.Context,
	namespacedName types.NamespacedName,
	service *v1.Service,
	listenPort int,
	protocol v1.Protocol,
	unDyingProxyName string,
) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		exists := false
		for index, existingPort := range serviceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxyName {
				exists = true
				serviceCopy.Spec.Ports[index].Port = int32(listenPort)
				serviceCopy.Spec.Ports[index].TargetPort = intstr.FromInt(listenPort)
				serviceCopy.Spec.Ports[index].Protocol = protocol
				break
			}
		}

		if !exists {
			portSpec := v1.ServicePort{
				Name:       unDyingProxyName,
				Port:       int32(listenPort),
				TargetPort: intstr.FromInt(listenPort),
				Protocol:   protocol,
			}
			serviceCopy.Spec.Ports = append(serviceCopy.Spec.Ports, portSpec)
		}

		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("failed to update (%s) Service to set Port: (%w)", service.Name, err)
	}

	return nil
}

func (r *UnDyingProxyReconciler) cleanupServiceForUnDyingProxy(
	ctx context.Context,
	unDyingProxy *proxyv1alpha1.UnDyingProxy,
	protocol v1.Protocol,
) error {
	protocolStr := "TCP"
	serviceName := r.TCPServiceToManage
	listenPort := unDyingProxy.Spec.TCP.ListenPort
	if protocol == v1.ProtocolUDP {
		protocolStr = "UDP"
		serviceName = r.UDPServiceToManage
		listenPort = unDyingProxy.Spec.UDP.ListenPort
	}

	log := ctx.Value(ctxLogger{}).(logr.Logger).WithValues(
		"protocol", protocolStr,
		"Service.Name", serviceName,
		"listenPort", listenPort,
	)

	service := &v1.Service{}
	namespacedName := types.NamespacedName{Name: serviceName, Namespace: unDyingProxy.Namespace}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := r.Get(ctx, namespacedName, service); err != nil {
			return err
		}
		serviceCopy := service.DeepCopy()

		for index, existingPort := range serviceCopy.Spec.Ports {
			if existingPort.Name == unDyingProxy.Name {
				serviceCopy.Spec.Ports = append(
					serviceCopy.Spec.Ports[:index],
					serviceCopy.Spec.Ports[index+1:]...)
				break
			}
		}

		return r.Patch(ctx, serviceCopy, client.MergeFrom(service), patchOptions)
	})
	if err != nil {
		return fmt.Errorf("failed to update (%s) Service to remove Port: (%w)", service.Name, err)
	}

	log.V(1).Info("Updated Service to remove listenPort")

	return nil
}
