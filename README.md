# tailscale-demo-env

This repo contains Pulumi infrastructure as code definitions for an AWS demo environment that shows various Tailscale uses. It also contains a wrapper CLI with Pulumi's automation API to automate the provisioning of the infrastructure.

It provisions to 3 AWS regions:

- us-west-2
- us-east-1
- eu-central-1

## Resources

The resources this repo provisions are:

- A VPC with public and private subnets
- A [Tailscale subnet router](https://tailscale.com/kb/1019/subnets) and exit node in a high-availability pair
- An [app connector](https://tailscale.com/kb/1281/app-connectors)
- A private EKS cluster with no public access
- [Karpenter](https://karpenter.sh/) to provision private node groups
- The [Tailscale Kubernetes operator](https://tailscale.com/kb/1236/kubernetes-operator) [set to auth mode](https://tailscale.com/kb/1236/kubernetes-operator#configuring-the-api-server-proxy-in-auth-mode)
- A subnet router configured with [4via6 for the Kubernetes service CIDR](https://tailscale.com/kb/1201/4via6-subnets)
- Prometheus with a [Tailscale ingress](https://tailscale.com/kb/1236/kubernetes-operator#ingress-resource) for private access to the web UI

## Usage

You can use the Automation API CLI to provision all the infrastructure. You'll need a Pulumi backend configured before this will work.

```bash
go run cli/main.go destroy --path $(pwd)/infrastructure/
```

You can get streamed JSON logs with

```bash
go run cli/main.go destroy --path $(pwd)/infrastructure/ --json
```

## Configuration

Stack configuration has been checked into this repo. If you'd like to be able to run this entire repo, please open an issue and I'll move the stack configuration out of the repo.

