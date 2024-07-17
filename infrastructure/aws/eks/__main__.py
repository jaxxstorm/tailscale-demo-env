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
    "tailscale_org": "lbrlabs.com",
}

VPC = pulumi.StackReference(f"lbrlabs58/lbr-demo-vpcs/{STACK}")
VPC_ID = VPC.get_output("vpc_id")
PUBLIC_SUBNET_IDS = VPC.require_output("public_subnet_ids")
PRIVATE_SUBNET_IDS = VPC.require_output("private_subnet_ids")

AWS_CONFIG = pulumi.Config("aws")
REGION = AWS_CONFIG.require("region")

CONFIG = pulumi.Config("")
ADMIN_ROLE_NAME = CONFIG.require("admin_role_name")


NAME = "-".join(REGION.split("-")[:2])

TAILSCALE_CONFIG = pulumi.Config("tailscale")
TAILSCALE_OAUTH_CLIENT_ID = TAILSCALE_CONFIG.require("oauth_client_id")
TAILSCALE_OAUTH_CLIENT_SECRET = TAILSCALE_CONFIG.require_secret("oauth_client_secret")

CONFIG = pulumi.Config()
SITE = CONFIG.require_int("site")
CLUSTER_ENDPOINT_PRIVATE_ACCESS = CONFIG.get_bool("cluster_endpoint_private_access", default=True)
CLUSTER_ENDPOINT_PUBLIC_ACCESS = CONFIG.get_bool("cluster_endpoint_public_access", default=True)

ADMIN_ACCESS_PRINCIPAL = aws.iam.get_role_output(name=ADMIN_ROLE_NAME)


cluster = eks.Cluster(
    f"lbr-{NAME}",
    cluster_subnet_ids=PRIVATE_SUBNET_IDS,
    system_node_subnet_ids=PRIVATE_SUBNET_IDS,
    system_node_instance_types=["t3.medium"],
    system_node_desired_count=2,
    cluster_endpoint_public_access=CLUSTER_ENDPOINT_PUBLIC_ACCESS,
    cluster_endpoint_private_access=CLUSTER_ENDPOINT_PRIVATE_ACCESS,
    enable_external_ingress=True,
    enable_internal_ingress=False,
    admin_access_principal=ADMIN_ACCESS_PRINCIPAL.arn,
    lets_encrypt_email="lets-encrypt@lbrlabs.com",
    tags=TAGS,
)

pulumi.export("kubeconfig", cluster.kubeconfig)
pulumi.export("cluster_name", cluster.cluster_name)

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
        repo="https://pkgs.tailscale.com/unstable/helmcharts",
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
            "defaultTags": [
                "tag:k8s-operator",
                f"tag:{STACK}",
            ],
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

admin_role = aws.iam.get_role_output(
    name="AWSReservedSSO_AdministratorAccess_d9b9fbdff66748e1",
)

eks.IamRoleMapping(
    "admins",
    username="admins",
    role_arn=admin_role.arn,
    groups=["system:masters"],
    opts=pulumi.ResourceOptions(parent=cluster, provider=provider),
)

sandbox_role = aws.iam.get_role_output(
    name="AWSReservedSSO_Sandbox_2a30cfafeae961b0",
)

eks.IamRoleMapping(
    "sandbox",
    username="sandbox",
    role_arn=sandbox_role.arn,
    groups=["system:masters"],
    opts=pulumi.ResourceOptions(parent=cluster, provider=provider),
)

ipv6_cidr = ip_calc.get_4via6_address(SITE, "10.100.0.0/16")

proxyclass = k8s.apiextensions.CustomResource(
    "metrics",
    kind="ProxyClass",
    api_version="tailscale.com/v1alpha1",
    spec={
        "metrics": {"enable": True},
        "statefulSet": {
            "pod": {
                "affinity": {
                    "podAntiAffinity": {
                        "requiredDuringSchedulingIgnoredDuringExecution": [
                            {
                                "labelSelector": {
                                    "matchExpressions": [
                                        {
                                            "key": "kubernetes.io/hostname",
                                            "operator": "Exists",
                                        }
                                    ]
                                },
                                "topologyKey": "kubernetes.io/hostname",
                            }
                        ]
                    }
                }
            }
        },
    },
    opts=pulumi.ResourceOptions(
        provider=provider, parent=provider, depends_on=[tailscale_operator]
    ),
)

pulumi.export("proxyclass", proxyclass.metadata["name"])

service_router = k8s.apiextensions.CustomResource(
    f"service-router-{NAME}",
    kind="Connector",
    api_version="tailscale.com/v1alpha1",
    spec={
        "hostname": f"eks-service-router-{NAME}",
        "subnetRouter": {"advertiseRoutes": [ipv6_cidr]},
    },
    opts=pulumi.ResourceOptions(
        provider=provider, parent=tailscale_operator, depends_on=[tailscale_operator]
    ),
)
