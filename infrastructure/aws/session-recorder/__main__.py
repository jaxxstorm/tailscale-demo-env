import pulumi
import pulumi_aws as aws
import pulumi_tailscale as tailscale
import json

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

ECS = pulumi.StackReference(f"lbrlabs58/ecs/{STACK}")
CLUSTER_ARN = ECS.require_output("cluster_arn")

AWS_CONFIG = pulumi.Config("aws")
REGION = AWS_CONFIG.require("region")


bucket = aws.s3.Bucket(
    "lbr-session-bucket",
    tags=TAGS,
)

aws.s3.BucketOwnershipControls(
    "lbr-session-bucket-ownership-controls",
    bucket=bucket.bucket,
    rule=aws.s3.BucketOwnershipControlsRuleArgs(
        object_ownership="BucketOwnerPreferred",
    ),
)

policy_doc = aws.iam.get_policy_document_output(
    statements=[
        aws.iam.GetPolicyDocumentStatementArgs(
            actions=[
                "s3:PutObject",
                "s3:GetBucketLocation",
                "s3:GetObject",
                "s3:ListBucket",
            ],
            effect="Allow",
            resources=[
                bucket.arn,
                pulumi.Output.concat(bucket.arn, "/*"),
            ],
        ),
        aws.iam.GetPolicyDocumentStatementArgs(
            actions=["ecr:*"],
            effect="Allow",
            resources=["*"],
        ),
    ]
)

policy = aws.iam.Policy(
    "lbr-session-bucket-policy",
    description="Allow S3 bucket access",
    policy=policy_doc.json,
)

log_group = aws.cloudwatch.LogGroup(
    "lbr-session-recorder",
    retention_in_days=3,
)

task_execution_role = aws.iam.Role(
    "lbr-session-recorder-task-exec-role",
    assume_role_policy=json.dumps(
        {
            "Version": "2008-10-17",
            "Statement": [
                {
                    "Sid": "",
                    "Effect": "Allow",
                    "Principal": {"Service": "ecs-tasks.amazonaws.com"},
                    "Action": "sts:AssumeRole",
                }
            ],
        }
    ),
)

aws.iam.RolePolicyAttachment(
    "lbr-session-recorder-ecs-policy-attachment",
    role=task_execution_role.name,
    policy_arn="arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy",
    opts=pulumi.ResourceOptions(parent=task_execution_role),
)

task_role = aws.iam.Role(
    "lbr-session-recorder",
    assume_role_policy=json.dumps(
        {
            "Version": "2012-10-17",
            "Statement": [
                {
                    "Action": "sts:AssumeRole",
                    "Principal": {"Service": "ecs-tasks.amazonaws.com"},
                    "Effect": "Allow",
                    "Sid": "",
                }
            ],
        }
    ),
)

aws.iam.RolePolicyAttachment(
    "lbr-session-recorder-ecs",
    role=task_role.name,
    policy_arn=aws.iam.ManagedPolicy.AMAZON_ECS_FULL_ACCESS,
    opts=pulumi.ResourceOptions(parent=task_role),
)

aws.iam.RolePolicyAttachment(
    "lbr-session-recorder-s3",
    role=task_role.name,
    policy_arn=policy.arn,
    opts=pulumi.ResourceOptions(parent=task_role),
)

repo = aws.ecr.get_repository_output(
    name="tsrecorder",
)

image = aws.ecr.get_image_output(
    repository_name=repo.name,
    most_recent=True,
)

ts_key = tailscale.TailnetKey(
    "lbr-session-recorder",
    ephemeral=True,
    reusable=True,
    preauthorized=True,
    tags=["tag:session-recorder"]
)

task_definition = aws.ecs.TaskDefinition(
    "lbr-session-recorder",
    family="lbr-session-recorder",
    cpu="256",
    memory="512",
    network_mode="awsvpc",
    execution_role_arn=task_execution_role.arn,
    task_role_arn=task_role.arn,
    requires_compatibilities=["FARGATE"],
    runtime_platform={"cpuArchitecture": "ARM64", "operatingSystemFamily": "LINUX"},
    container_definitions=pulumi.Output.json_dumps(
        [
            {
                "name": "tsrecorder",
                "image": pulumi.Output.format(
                    "{0}@{1}", repo.repository_url, image.image_digest
                ),
                "environment": [
                    {
                        "name": "TSRECORDER_DST",
                        "value": f"s3://s3.{REGION}.amazonaws.com",
                    },
                    {
                        "name": "TSRECORDER_BUCKET",
                        "value": bucket.bucket,
                    },
                    {
                        "name": "TS_AUTHKEY",
                        "value": ts_key.key,
                    },
                ],
                "command": ["/tsrecorder", "--statedir=/data/state", "--ui"],
                "logConfiguration": {
                    "logDriver": "awslogs",
                    "options": {
                        "awslogs-group": log_group.id,
                        "awslogs-region": aws.get_region().name,
                        "awslogs-stream-prefix": pulumi.Output.concat(
                            "lbr-session-recorder"
                        ),
                    },
                },
            }
        ]
    ),
    tags=TAGS,
)

security_group = aws.ec2.SecurityGroup(
    "lbr-session-recorder",
    vpc_id=VPC_ID,
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

service = aws.ecs.Service(
    "lbr-session-recorder",
    cluster=CLUSTER_ARN,
    desired_count=1,
    launch_type="FARGATE",
    task_definition=task_definition.arn,
    network_configuration=aws.ecs.ServiceNetworkConfigurationArgs(
        security_groups=[security_group.id],
        assign_public_ip=False,
        subnets=PRIVATE_SUBNET_IDS,
    ),
    tags=TAGS,
    opts=pulumi.ResourceOptions(parent=task_definition),
)

pulumi.export("bucket_name", bucket.bucket)
