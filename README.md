# UnDyingProxy is a UDP forwarder for Kubernetes

## Why?
Cloud Loadbalancers can be expensive. This operator forwards UDP traffic from a single IP and many ports, to many destinations, one destination per source port for now. It is designed to be run as multiple replicas to provide high availability.

Each `UnDyingProxy` object specifies one listen port, and one target host and port to forward to. E.G.
```yaml
---
apiVersion: proxy.sfact.io/v1alpha1
kind: UnDyingProxy
metadata:
  name: example
  namespace: undying-proxy
  labels:
    app: undying-proxy
spec:
  listenPort: 1234
  targetPort: 1234
  targetHost: my-app.example.svc.cluster.local
```

This supports only paying for one Loadbalancer, or even avoiding a cloud Loadbalancer altogether. If using a cloud LoadBalancer, this operator can automatically add/remove `Ports` in a `Service` object as `UnDyingProxy` objects are created/destroyed, to open/close the ports in the LoadBalancer.


## Description
UnDyingProxy listens to UDP ports and forward packets to destination addresses and ports. Configuration is done via UnDyingProxy objects, one for each port to be forwarded. This operator works like the NGINX Ingress Controller in that it does the actual forwarding itself, rather than operating external forwarders. Therefore, it should be run as multiple replicas to provide high availability. Leader election is disabled. Currently, each listener supports forwarding to a single destination address and port.

The `--operator-namespace` flag must be set, as this is a namespaced operator. The operator will only watch for UnDyingProxy objects in the namespace specified by this flag. The operator can manage a Kubernetes `Service` object's Ports to dynamically support new ingress for each UnDyingProxy, for example through a cloud provider's LoadBalancer.

Run this operator once per IP to listen upon. TODO, support an annotation to set which instance of the operator will handle a given UnDyingProxy, which will allow for more than one operator in the same namespace.

Use `config/manager` as example configs to customize.

## TODO curate the rest of this auto-generated README
- TODO We shouldn't leave a goroutine running forever for every client that ever connects. UDP is connectionless though, so we have to use readDeadline for evenutually shutting down the goroutines for that client, when no new packets have been received in a while.
  - The fromClientsToTarget goroutine needs to stay running forever,
  - The fromTargetToClients goroutines need to be shut down after a timeout.
- TODO try switching shutdown from listener first to listener last
- TODO test if recording these metrics slows things down or not
- TODO also mutex might be slowing it down too much
- TODO consider trying atomic like described here: https://stackoverflow.com/a/57963829/2971199

## Getting Started

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/undying-proxy:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/undying-proxy:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following are the steps to build the installer and distribute this project to users.

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/undying-proxy:tag
```

NOTE: The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without
its dependencies.

2. Using the installer

Users can just run kubectl apply -f <URL for YAML BUNDLE> to install the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/undying-proxy/<tag or branch>/dist/install.yaml
```

## Contributing
// TODO(user): Add detailed information on how you would like others to contribute to this project

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

## License

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

