# Install with kubectl

```sh
kubectl apply -f dist/install.yaml
```

`dist/install.yaml` is the kustomize build of `config/default` (CRDs, RBAC,
manager Deployment). Regenerate with `make build-installer`.
