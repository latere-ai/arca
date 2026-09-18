# Bootstrap resources

The release pipeline never reads this directory. Everything it applies runs
again on every release, so everything it applies must be re-appliable, and
the resources here are not: a namespace is created once, a Secret holds
values that must not be in git, and a Job's pod template cannot be changed
in place.

```sh
kubectl apply -f deploy/bootstrap/namespace.yaml

cp deploy/bootstrap/secrets.example.yaml /tmp/arcad-secrets.yaml
# fill in the bucket, the database, and the issuers, then
kubectl apply -f /tmp/arcad-secrets.yaml
rm /tmp/arcad-secrets.yaml
```

Then, before the first rollout and before every upgrade:

```sh
kubectl -n arca delete job arcad-migrate --ignore-not-found
kubectl -n arca apply -f deploy/bootstrap/migrate-job.yaml
kubectl -n arca wait --for=condition=complete job/arcad-migrate --timeout=300s
```

Set the Job's image to the release you are installing first. Migrations are
forward-only and a server refuses to serve against a schema below its own,
so a rollout that skipped the Job fails its readiness probe instead of
serving against the wrong shape.

[`../../docs/install.md`](../../docs/install.md) is the whole walk from an
empty cluster to a serving installation, and
[`../../docs/operations.md`](../../docs/operations.md) is what to do after
it.

A resource that belongs to a release belongs in [`../base`](../base) with
the installation's own values in an overlay, not here.
