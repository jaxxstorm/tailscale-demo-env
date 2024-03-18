import pulumi
import pulumi_aws as aws
import pulumi_kubernetes as k8s
import lbrlabs_pulumi_eks as eks
import ip_calc

PROJECT_NAME = pulumi.get_project()
STACK = pulumi.get_stack()

TAGS = {
    "environment": STACK,
    "project": PROJECT_NAME,
    "owner": "lbriggs",
    "deployed_by": "pulumi",
    "org": "lbrlabs",
}

VPC = pulumi.StackReference(f"lbrlabs58/tailscale-demo-vpcs/{STACK}")
VPC_ID = VPC.get_output("vpc_id")
PUBLIC_SUBNET_IDS = VPC.get_output("public_subnet_ids")
PRIVATE_SUBNET_IDS = VPC.get_output("private_subnet_ids")

AWS_CONFIG = pulumi.Config("aws")
REGION = AWS_CONFIG.require("region")
NAME = "-".join(REGION.split("-")[:2])

TAILSCALE_CONFIG = pulumi.Config("tailscale")
TAILSCALE_OAUTH_CLIENT_ID = TAILSCALE_CONFIG.require("oauth_client_id")
TAILSCALE_OAUTH_CLIENT_SECRET = TAILSCALE_CONFIG.require_secret("oauth_client_secret")

# create a cluster
# no internal ingress controller
# fully private cluster
cluster = eks.Cluster(
    f"lbr-{NAME}",
    cluster_subnet_ids=PRIVATE_SUBNET_IDS,
    cluster_endpoint_private_access=True,
    cluster_endpoint_public_access=False,
    system_node_subnet_ids=PRIVATE_SUBNET_IDS,
    system_node_instance_types=["t3.medium"],
    system_node_desired_count=2,
    enable_external_ingress=True,
    enable_internal_ingress=False,
    lets_encrypt_email="lets-encrypt@lbrlabs.com",
)

# retrieve the security group used for node to node communitation
sg = cluster.control_plane.vpc_config.cluster_security_group_id
vpc = aws.ec2.get_vpc_output(id=VPC_ID)

# allow all access from inside the VPC cidr
ingress = aws.ec2.SecurityGroupRule(
    f"lbr-{NAME}-allow-vpc-traffic",
    type="ingress",
    to_port=0,
    from_port=0,
    protocol="all",
    security_group_id=sg,
    cidr_blocks=[vpc.cidr_block],
)

# create a provider
# we need to wait for the ingress sg rule so we can use it
provider = k8s.Provider(
    f"lbr-{NAME}",
    kubeconfig=cluster.kubeconfig,
    opts=pulumi.ResourceOptions(depends_on=[ingress]),
)
pulumi.export("kubeconfig", cluster.kubeconfig)

# create a karpenter autoscaling group
workload = eks.AutoscaledNodeGroup(
    f"lbr-{NAME}-private",
    node_role=cluster.karpenter_node_role.name,
    security_group_ids=[cluster.control_plane.vpc_config.cluster_security_group_id],
    subnet_ids=PRIVATE_SUBNET_IDS,
    requirements=[
        eks.RequirementArgs(
            key="kubernetes.io/arch",
            operator="In",
            values=["amd64"],
        ),
        eks.RequirementArgs(
            key="kubernetes.io/os",
            operator="In",
            values=["linux"],
        ),
        eks.RequirementArgs(
            key="karpenter.k8s.aws/instance-family",
            operator="In",
            values=["t3"],
        ),
        eks.RequirementArgs(
            key="karpenter.k8s.aws/instance-size",
            operator="In",
            values=["medium"],
        ),
        eks.RequirementArgs(
            key="karpenter.sh/capacity-type",
            operator="In",
            values=["spot"],
        ),
    ],
    opts=pulumi.ResourceOptions(
        provider=provider,
    ),
)

tailscale_ns = k8s.core.v1.Namespace(
    f"tailscale-ns",
    metadata=k8s.meta.v1.ObjectMetaArgs(name="tailscale"),
    opts=pulumi.ResourceOptions(provider=provider, parent=provider),
)

tailscale_operator = k8s.helm.v3.Release(
    "tailscale",
    repository_opts=k8s.helm.v3.RepositoryOptsArgs(
        repo="https://pkgs.tailscale.com/helmcharts",
    ),
    namespace=tailscale_ns.metadata.name,
    chart="tailscale-operator",
    values={
        "oauth": {
            "clientId": TAILSCALE_OAUTH_CLIENT_ID,
            "clientSecret": TAILSCALE_OAUTH_CLIENT_SECRET,
        },
        "apiServerProxyConfig": {
            "mode": "true",
        },
        "operatorConfig": {
            "hostname": f"eks-operator-{STACK}",
            "tolerations": [
                {
                    "key": "node.lbrlabs.com/system",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                },
            ],
        },
    },
    opts=pulumi.ResourceOptions(provider=provider, parent=tailscale_ns),
)

monitoring_ns = k8s.core.v1.Namespace(
    "monitoring",
    metadata=k8s.meta.v1.ObjectMetaArgs(
        name="monitoring",
    ),
    opts=pulumi.ResourceOptions(provider=provider),
)

kube_prometheus = k8s.helm.v3.Release(
    "kube-prometheus",
    repository_opts=k8s.helm.v3.RepositoryOptsArgs(
        repo="https://prometheus-community.github.io/helm-charts",
    ),
    chart="kube-prometheus-stack",
    namespace=monitoring_ns.metadata.name,
    version="57.0.1",
    values={
        "grafana": {
            "enabled": False,
        },
        "prometheus-node-exporter": {
            "affinity": {
                "nodeAffinity": {
                    "requiredDuringSchedulingIgnoredDuringExecution": {
                        "nodeSelectorTerms": [
                            {
                                "matchExpressions": [
                                    {
                                        "key": "eks.amazonaws.com/compute-type",
                                        "operator": "NotIn",
                                        "values": ["fargate"],
                                    }
                                ]
                            }
                        ]
                    }
                }
            }
        },
        "alertmanager": {
            "alertmanagerSpec": {
                "tolerations": [
                    {
                        "key": "node.lbrlabs.com/system",
                        "operator": "Equal",
                        "value": "true",
                        "effect": "NoSchedule",
                    },
                ],
            },
        },
        "admissionWebhooks": {
            "patch": {
                "tolerations": [
                    {
                        "key": "node.lbrlabs.com/system",
                        "operator": "Equal",
                        "value": "true",
                        "effect": "NoSchedule",
                    },
                ],
            }
        },
        "kubeStateMetrics": {
            "tolerations": [
                {
                    "key": "node.lbrlabs.com/system",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                },
            ],
        },
        "nodeExporter": {
            "tolerations": [
                {
                    "key": "node.lbrlabs.com/system",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                }
            ],
        },
        "prometheus": {
            "ingress": {
                "enabled": True,
                "hosts": [f"prometheus-{NAME}"],
                "ingressClassName": "tailscale",
                "tls": [
                    {
                        "hosts": [f"prometheus-{NAME}"],
                    }
                ],
            },
            "prometheusSpec": {
                "externalLabels": {
                    "cluster": cluster.cluster_name,
                },
                "serviceMonitorSelector": {},
                "serviceMonitorSelectorNilUsesHelmValues": False,
                "tolerations": [
                    {
                        "key": "node.lbrlabs.com/system",
                        "operator": "Equal",
                        "value": "true",
                        "effect": "NoSchedule",
                    }
                ],
            },
        },
    },
    opts=pulumi.ResourceOptions(
        parent=monitoring_ns, provider=provider, depends_on=[tailscale_operator]
    ),
)


# ipv6_cidr = ip_calc.get_4via6_address(1, "10.100.0.0/16")

# service_router = k8s.apiextensions.CustomResource(
#     f"service-router-{STACK}",
#     kind="Connector",
#     api_version="tailscale.com/v1alpha1",
#     spec={
#         "hostname": f"eks-service-router-{STACK}",
#         "subnetRouter": {
#             "advertiseRoutes": [ "10.100.0.0/16" ]
#         }
#     },
#     opts=pulumi.ResourceOptions(provider=provider, parent=provider, depends_on=[tailscale_operator]),
# )