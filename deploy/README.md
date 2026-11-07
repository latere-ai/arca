# Deploy

Kustomize manifests for an installation of `arcad`, the example overlays
an installation starts from, and the maintainer's own installation.
[`docs/install.md`](../docs/install.md) walks an installation from an
empty cluster to a serving one; start there.

| Directory | Holds |
|---|---|
| `base/` | what `arcad` needs in any installation: the API Deployment and the reconciler Deployment, the Service, the ServiceAccount, the NetworkPolicy, the PodDisruptionBudget, the HorizontalPodAutoscaler, and a PrometheusRule beside them that the base does not list, because it needs the Prometheus operator. The base names no namespace and no Ingress; an overlay supplies both |
| `bootstrap/` | what is applied once by hand and never by a release: the Namespace, the Secret template for the bucket, the database and the issuers, the migration Job that runs before the first rollout and before every upgrade, and the identity a pipeline deploys with. Its [README](bootstrap/README.md) gives the order |
| `examples/` | overlays of the base to copy and edit: `kind/` is a whole installation on one laptop node with a bucket, a database and stub identity services, `aws/` is an installation on EKS, and `digitalocean/` is one on DigitalOcean Kubernetes. Each names the files to change in its header |
| `prod/` | the maintainer's own installation, not an install path: the base with the maintainer's ingress, public address, authorizer, replica counts and network policy for its database. A release applies it through the `deploy` job of `.github/workflows/release.yml` only where the repository variable `ARCA_RELEASE_DEPLOY` is set, so a fork deploys nothing. It holds only manifests that are safe to apply again |

The release workflow renders `base/`, each example and `prod/` with
`kubectl kustomize`. No cloud account runs the `aws/` and
`digitalocean/` overlays.
