See [e2e-node-tests](https://git.k8s.io/community/contributors/devel/sig-node/e2e-node-tests.md)

To run the node-e2e local flow inside a privileged container (no host
containerd / CNI / etcd install required), see
[dockerized/README.md](dockerized/README.md) (`make test-e2e-node DOCKER=true`).
