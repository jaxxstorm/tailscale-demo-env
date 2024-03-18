import pulumi_aws as aws
import pulumi_kubernetes as k8s
import pulumi_random as random
import pulumi

PROJECT_NAME = pulumi.get_project()
STACK = pulumi.get_stack()

TAGS = {
    "environment": STACK,
    "project": PROJECT_NAME,
    "owner": "lbriggs",
    "deployed_by": "pulumi",
    "org": "lbrlabs",
}

CLUSTER = pulumi.StackReference(f"lbrlabs58/tailscale-demo-eks/{STACK}")
CLUSTER_NAME = CLUSTER.get_output("cluster_name")
KUBECONFIG = CLUSTER.get_output("kubeconfig")

AWS_CONFIG = pulumi.Config("aws")
REGION = AWS_CONFIG.require("region")
NAME = "-".join(REGION.split("-")[:2])

CONFIG = pulumi.Config()
GRAFANA_ENABLED = CONFIG.get_bool("grafana_enabled")

provider = k8s.Provider(
    f"lbr-{NAME}",
    kubeconfig=KUBECONFIG,
    opts=pulumi.ResourceOptions(parent=CLUSTER),
)

monitoring_ns = k8s.core.v1.Namespace(
    "monitoring",
    metadata=k8s.meta.v1.ObjectMetaArgs(
        name="monitoring",
    ),
    opts=pulumi.ResourceOptions(provider=provider),
)

if GRAFANA_ENABLED:
    VPC = pulumi.StackReference(f"lbrlabs58/tailscale-demo-vpcs/{STACK}")
    VPC_ID = VPC.get_output("vpc_id")
    PRIVATE_SUBNET_IDS = VPC.get_output("private_subnet_ids")

    subnet_group = aws.rds.SubnetGroup(
        f"lbr-{NAME}",
        description=f"lbr demo env: Subnet group for grafana monitoring",
        subnet_ids=PRIVATE_SUBNET_IDS,
        tags=TAGS,
    )

    # get the vpc so we can know the cidr block:
    vpc = aws.ec2.get_vpc(id=VPC_ID)

    security_group = aws.ec2.SecurityGroup(
        f"lbr-{NAME}-db-sg",
        description=f"Security group for lbr grafana database",
        vpc_id=VPC_ID,
        ingress=[
            aws.ec2.SecurityGroupIngressArgs(
                protocol="tcp",
                from_port=5432,
                to_port=5432,
                cidr_blocks=[vpc.cidr_block],
            )
        ],
        egress=[
            aws.ec2.SecurityGroupEgressArgs(
                protocol="-1",
                from_port=0,
                to_port=0,
                cidr_blocks=["0.0.0.0/0"],
            )
        ],
        tags=TAGS,
    )

    db_password = random.RandomPassword(
        f"lbr-{NAME}-db-password",
        length=32,
        special=False,
    )

    db = aws.rds.Instance(
        f"lbr-{NAME}-grafana",
        db_subnet_group_name=subnet_group.name,
        allocated_storage=20,
        max_allocated_storage=100,
        copy_tags_to_snapshot=True,
        db_name="grafana",
        engine="postgres",
        instance_class="db.t4g.micro",
        engine_version="13.10",
        vpc_security_group_ids=[security_group.id],
        username="grafana",
        password=db_password.result,
        tags=TAGS,
        skip_final_snapshot=True,
    )

    db_secret = k8s.core.v1.Secret(
        "grafana-db-secret",
        metadata=k8s.meta.v1.ObjectMetaArgs(
            name="grafana-db-secret",
            namespace=monitoring_ns.metadata.name,
        ),
        string_data={
            "PASSWORD": db_password.result,
        },
        opts=pulumi.ResourceOptions(provider=provider, parent=monitoring_ns),
    )
    
    grafana_config = {
            "enabled": GRAFANA_ENABLED,
            "ingress": {
                "enabled": True,
                "hosts": [f"grafana"],
                "ingressClassName": "tailscale",
                "annotations": {
                    "tailscale.com/funnel": "true",
                },
                "tls": [
                    {
                        "hosts": [f"grafana"],
                    }
                ],
            },
            "sidecar": {
                "datasources": {
                    "uid": f"prometheus-{NAME}"
                }
            },
            "env": {
                "GF_DATABASE_TYPE": "postgres",
                "GF_DATABASE_HOST": db.endpoint,
                "GF_DATABASE_USER": "grafana",
                "GF_DATABASE_NAME": "grafana",
                "GF_DATABASE_SSL_MODE": "require",
            },
            "envValueFrom": {
                "GF_DATABASE_PASSWORD": {
                    "secretKeyRef": {
                        "name": db_secret.metadata.name,
                        "key": "PASSWORD",
                    }
                },
            },
            "tolerations": [
                {
                    "key": "node.lbrlabs.com/system",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                },
            ],
        }
else:
    grafana_config = {"enabled": GRAFANA_ENABLED}

kube_prometheus = k8s.helm.v3.Release(
    "kube-prometheus",
    repository_opts=k8s.helm.v3.RepositoryOptsArgs(
        repo="https://prometheus-community.github.io/helm-charts",
    ),
    chart="kube-prometheus-stack",
    namespace=monitoring_ns.metadata.name,
    version="57.0.1",
    values={
        "grafana": grafana_config,
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
                    "cluster": CLUSTER_NAME,
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
    opts=pulumi.ResourceOptions(parent=monitoring_ns, provider=provider),
)
