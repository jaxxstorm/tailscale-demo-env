import pulumi
import pulumi_azure as azure

STACK = pulumi.get_stack()
PROJECT = pulumi.get_project()
LOCATION = "WestUS2"

tags = {
    "project": PROJECT,
    "stack": STACK,
}

resource_group = azure.core.ResourceGroup(
    STACK,
    location=LOCATION,
)

pulumi.export("resource_group_name", resource_group.name)
pulumi.export("resource_group_location", resource_group.location)
pulumi.export("resource_group_id", resource_group.id)