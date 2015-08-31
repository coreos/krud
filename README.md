# krud

krud is a kubernetes rolling-update service for use with docker registry webhook pushes.

For example, when you push a new image to Quay.io, you can configure a HTTP POST webhook.
When krud gets the webhook, it does a rolling update of a kubernetes replication controller (almost identical to `kubectl rolling-update`).
krud can run in a separate kubernetes service along side your other services.

# Usage

Configure the webhook to connect to `/push` at the address it's running, for example: `http://krud.company.com/push`.

A bad UI exists at `/` showing status of past update attempts.

# Configuration

The following environment variables are used to configure krud:

Required:

- `KRUD_CONTROLLER_NAME`: name of the replication controller to update

Optional:

- `KRUD_DEPLOYMENT_KEY`: key to use to differentiate between two different controllers; defaults to `deployment`
- `KRUD_K8S_ENDPOINT`: kubernetes endpoint; defaults to `http://localhost:8080`
- `KRUD_LISTEN`: listen address; defaults to `:9500`

These options can also be specified on the command line. See `krud -help` for usage.

# Supported Registries

- [Docker Hub](https://hub.docker.com/)
- [Quay.io](https://quay.io)

# Running on kubernetes

There are example `rc.yaml` and `svc.yaml` files.
The only required change is setting the `KRUD_CONTROLLER_NAME` environment variable in `rc.yaml`.
