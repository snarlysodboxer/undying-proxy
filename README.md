# UnDyingProxy is a TCP/UDP forwarder for Kubernetes

## Why?

Cloud load balancers can be expensive. This operator forwards TCP and UDP traffic from a single IP and many ports, to many destinations, one destination per source port for now. It is designed to be run as multiple replicas to provide high availability.

Each `UnDyingProxy` object specifies one listen port and one target host and port to forward to each for TCP and UDP. E.G.

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
  tcp:
    listenPort: 1234
    targetPort: 1234
    targetHost: my-app.example.svc.cluster.local
  udp:
    listenPort: 1234
    targetPort: 1234
    targetHost: my-app.example.svc.cluster.local
```

This supports only paying for one Loadbalancer, or even avoiding a cloud Loadbalancer altogether. If using a cloud Loadbalancer, this operator can automatically add/remove `Ports` in a `Service` object as `UnDyingProxy` objects are created/destroyed, to open/close the ports in the Loadbalancer.

For example:

- Run many game servers on a single IP, each on a different port, all behind a single cloud Loadbalancer.
- Run as a DaemonSet, setup DNS to point to your node IPs, and avoid a cloud Loadbalancer altogether.

## Description

UnDyingProxy listens for TCP and UDP connections on designated ports, and forwards packets to destination addresses and ports. Configuration is done via UnDyingProxy objects, one for each port to be forwarded. This operator works like the NGINX Ingress Controller in that it does the actual forwarding itself, rather than operating external forwarders. Therefore, it should be run as multiple replicas to provide high availability. Leader election is disabled. Currently, each listener supports forwarding to a single destination address and port.

The `--operator-namespace` flag must be set, as this is a namespaced operator. The operator will only watch for UnDyingProxy objects in the namespace specified by this flag. The operator can manage a Kubernetes `Service` object's Ports to dynamically support new ingress for each UnDyingProxy, for example through a cloud provider's Loadbalancer.

Run one set of replicas of this operator per IP to listen upon.

Use `config/manager` as example configs to customize.

## Architecture

The operator watches for UnDyingProxy objects in the namespace specified by the `--operator-namespace` flag. When an UnDyingProxy object is created, the operator will start new goroutines listening on the specified ports for TCP and UDP traffic. When an UnDyingProxy object is deleted, the operator will shutdown those goroutines, and stop listening on the specified ports, and optionally edit Kubernetes Service objects to control cloud load balancers.

## TODOs

- Support one operator for UnDyingProxies from many namespaces.
- Support an annotation to set which instance of the operator will handle a given UnDyingProxy, which will allow for more than one operator in the same namespace.

## Getting Started

### Prerequisites

- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on a cluster

**Build and push an image somewhere:**

```sh
make docker-build docker-push IMG=<some-registry>/undying-proxy:tag
```

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/undying-proxy:tag
```

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

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

## Contributing

Contributions welcome! Please create an issue and fork the repository and submit a pull request.

**NOTE:** Run `make help` for more information on all potential `make` targets

More information can be found via the [Kubebuilder Documentation](https://book.kubebuilder.io/introduction.html)

Note the following:

- `make test`
  - Runs the unit tests.
- `make test-e2e`
  - Runs the end to end tests against an existing kubectl context named `kind-undying-proxy-test`
    - Tests are destructive. A check ensures you are connected to the correct context.
    - An easy way to create the cluster and context is with [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installing-with-a-package-manager): `kind create cluster --name undying-proxy-test`

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
