import pulumi
import pulumi_azure as azure

STACK = pulumi.get_stack()
PROJECT = pulumi.get_project()

CONFIG = pulumi.Config()
cidr_block = CONFIG.require("cidr_block")

STACK_REF = pulumi.StackReference(f"lbrlabs58/resource_group/{STACK}")
RESOURCE_GROUP = STACK_REF.get_output("resource_group_name")
RESOURCE_GROUP_NAME = STACK_REF.get_output("resource_group_name")
LOCATION = STACK_REF.get_output("resource_group_location")

tags = {
    "project": PROJECT,
    "stack": STACK,
}

vnet = azure.network.VirtualNetwork(
    STACK,
    location=LOCATION,
    resource_group_name=RESOURCE_GROUP_NAME,
    address_spaces=[
        cidr_block,
    ],
    tags=tags,
)

node_subnet = azure.network.Subnet(
    f"{STACK}-nodes",
    address_prefixes=[
        "172.20.252.0/24",
    ],
    resource_group_name=RESOURCE_GROUP_NAME,
    virtual_network_name=vnet.name,
    opts=pulumi.ResourceOptions(parent=vnet),
)

pod_subnet = azure.network.Subnet(
    f"{STACK}-pods",
    address_prefixes=[
        "172.20.0.0/22",
    ],
    resource_group_name=RESOURCE_GROUP_NAME,
    virtual_network_name=vnet.name,
    opts=pulumi.ResourceOptions(parent=vnet),
)



ip = azure.network.PublicIpPrefix(
    STACK,
    location=LOCATION,
    resource_group_name=RESOURCE_GROUP_NAME,
    prefix_length=30,
    tags=tags,
)

nat_gateway = azure.network.NatGateway(
    STACK,
    location=LOCATION,
    resource_group_name=RESOURCE_GROUP_NAME,
    tags=tags,
)

azure.network.NatGatewayPublicIpPrefixAssociation(
    STACK,
    nat_gateway_id=nat_gateway.id,
    public_ip_prefix_id=ip.id,
    opts=pulumi.ResourceOptions(parent=nat_gateway),
)

azure.network.SubnetNatGatewayAssociation(
    f"{STACK}-pods",
    nat_gateway_id=nat_gateway.id,
    subnet_id=pod_subnet.id,
    opts=pulumi.ResourceOptions(parent=nat_gateway),
)

azure.network.SubnetNatGatewayAssociation(
    f"{STACK}-nodes",
    nat_gateway_id=nat_gateway.id,
    subnet_id=node_subnet.id,
    opts=pulumi.ResourceOptions(parent=nat_gateway),
)


pulumi.export("vnet_name", vnet.name)
pulumi.export("vnet_id", vnet.id)
pulumi.export("node_subnet_name", node_subnet.name)
pulumi.export("node_subnet_id", node_subnet.id)
pulumi.export("pod_subnet_name", pod_subnet.name)
pulumi.export("pod_subnet_id", pod_subnet.id)