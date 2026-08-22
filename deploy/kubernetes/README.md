# Running Veritix on Kubernetes

A kustomize base for one Veritix instance. `docs/deployment.md` is the
reasoning; this is the manifest set.

```sh
kubectl create namespace veritix
kubectl -n veritix create secret generic veritix-auth \
    --from-literal=auth-token="$(openssl rand -hex 16)"
kubectl apply -k deploy/kubernetes
```

Then reach it however your cluster exposes things — `kubectl port-forward
svc/veritix 8080:80` to try it, an Ingress or Gateway with TLS for real use.
The Service is deliberately `ClusterIP`: Veritix holds the data the customer
would not send to a vendor, and nothing here should put it on the internet
without somebody deciding to.

Three things in here are load-bearing and are explained where they appear:
`replicas: 1`, the read-only root filesystem, and a NetworkPolicy that denies
egress by default.
