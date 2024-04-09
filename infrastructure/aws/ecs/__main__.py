import pulumi
import pulumi_aws as aws

PROJECT_NAME = pulumi.get_project()
STACK = pulumi.get_stack()

TAGS = {
    "environment": STACK,
    "project": PROJECT_NAME,
    "owner": "lbriggs",
    "deployed_by": "pulumi",
    "tailscale_org": "lbrlabs.com",
}

cluster = aws.ecs.Cluster(
    "lbr-cluster",
    tags=TAGS,
)

pulumi.export("cluster_name", cluster.name)
pulumi.export("cluster_arn", cluster.arn)