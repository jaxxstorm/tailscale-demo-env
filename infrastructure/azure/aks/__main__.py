import pulumi
import pulumi_azure as azure
import pulumi_tls as tls
import pulumi_kubernetes as k8s

STACK = pulumi.get_stack()
PROJECT = pulumi.get_project()

RG = pulumi.StackReference(f"lbrlabs58/resource_group/{STACK}")
RESOURCE_GROUP = RG.get_output("resource_group_name")
RESOURCE_GROUP_NAME = RG.get_output("resource_group_name")
LOCATION = RG.get_output("resource_group_location")

VNET = pulumi.StackReference(f"lbrlabs58/azure_vnet/{STACK}")
NODE_SUBNET_ID = VNET.get_output("node_subnet_id")
NODE_SUBNET_NAME = VNET.get_output("node_subnet_name")
POD_SUBNET_ID = VNET.get_output("pod_subnet_id")
POD_SUBNET_NAME = VNET.get_output("pod_subnet_name")

CONFIG = pulumi.Config()


TAILSCALE_CONFIG = pulumi.Config("tailscale")
TAILSCALE_OAUTH_CLIENT_ID = TAILSCALE_CONFIG.require("oauth_client_id")
TAILSCALE_OAUTH_CLIENT_SECRET = TAILSCALE_CONFIG.require_secret("oauth_client_secret")

tags = {
    "project": PROJECT,
    "stack": STACK,
}

ssh_key = tls.PrivateKey(
    f"{STACK}-aks-node-ssh-key",
    algorithm="RSA",
    rsa_bits=4096,
)

cluster = azure.containerservice.KubernetesCluster(
    STACK,
    resource_group_name=RESOURCE_GROUP_NAME,
    location=LOCATION,
    dns_prefix=f"{PROJECT}-{STACK}",
    network_profile=azure.containerservice.KubernetesClusterNetworkProfileArgs(
        network_plugin="azure"
    ),
    sku_tier="Standard",
    linux_profile=azure.containerservice.KubernetesClusterLinuxProfileArgs(
        admin_username="aks",
        ssh_key=azure.containerservice.KubernetesClusterLinuxProfileSshKeyArgs(
            key_data=ssh_key.public_key_openssh,
        ),
    ),
    storage_profile=azure.containerservice.KubernetesClusterStorageProfileArgs(
        blob_driver_enabled=True,
        snapshot_controller_enabled=True,
        file_driver_enabled=True,
    ),
    workload_identity_enabled=True,
    oidc_issuer_enabled=True,
    local_account_disabled=False,
    azure_policy_enabled=False,
    http_application_routing_enabled=False,
    identity=azure.containerservice.KubernetesClusterIdentityArgs(
        type="SystemAssigned"
    ),
    default_node_pool=azure.containerservice.KubernetesClusterDefaultNodePoolArgs(
        name="system",
        node_count=2,
        max_count=5,
        min_count=2,
        vm_size="Standard_B2ms",
        enable_auto_scaling=True,
        tags=tags,
        temporary_name_for_rotation="temp",
        only_critical_addons_enabled=True,
        vnet_subnet_id=NODE_SUBNET_ID,
        pod_subnet_id=POD_SUBNET_ID,
        upgrade_settings=azure.containerservice.KubernetesClusterDefaultNodePoolUpgradeSettingsArgs(
            max_surge="10%",
        )
    ),
    tags=tags,
    opts=pulumi.ResourceOptions(ignore_changes=["defaultNodePool.nodeCount"]),
)

app_nodes = azure.containerservice.KubernetesClusterNodePool(
    "app",
    kubernetes_cluster_id=cluster.id,
    vnet_subnet_id=NODE_SUBNET_ID,
    vm_size="Standard_B2ms",
    pod_subnet_id=POD_SUBNET_ID,
    enable_auto_scaling=True,
    node_count=1,
    min_count=3,
    max_count=5,
    tags=tags,
    opts=pulumi.ResourceOptions(parent=cluster, ignore_changes=["nodeCount"]),
)

k8s_provider = k8s.Provider(
    f"{STACK}-aks",
    kubeconfig=cluster.kube_config_raw,
)

tailscale_ns = k8s.core.v1.Namespace(
    "tailscale",
    metadata=k8s.meta.v1.ObjectMetaArgs(name="tailscale"),
    opts=pulumi.ResourceOptions(provider=k8s_provider, parent=k8s_provider),
)

tailscale_operator = k8s.helm.v3.Release(
    "tailscale",
    repository_opts=k8s.helm.v3.RepositoryOptsArgs(
        repo="https://pkgs.tailscale.com/helmcharts",
    ),
    version="1.61.11",
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
            "image": {
                "repo": "gcr.io/csi-test-290908/operator",
                "tag": "v0.0.1noacceptroutes",
            },
            "hostname": f"aks-operator-{STACK}",
        },
    },
    opts=pulumi.ResourceOptions(provider=k8s_provider, parent=tailscale_ns),
)

service_router = k8s.apiextensions.CustomResource(
    f"service-router-{STACK}",
    kind="Connector",
    api_version="tailscale.com/v1alpha1",
    spec={
        "hostname": f"aks-service-router-{STACK}",
        "subnetRouter": {
            "advertiseRoutes": [ "10.0.0.0/16" ]
        }
    },
    opts=pulumi.ResourceOptions(provider=k8s_provider, parent=k8s_provider, depends_on=[tailscale_operator]),
)



# ---------------------------------------------------------------------------------------
# Prometheus
# ---------------------------------------------------------------------------------------

monitoring_ns = k8s.core.v1.Namespace(
    "monitoring",
    metadata=k8s.meta.v1.ObjectMetaArgs(
        name="monitoring",
    ),
    opts=pulumi.ResourceOptions(provider=k8s_provider),
)

service_account = k8s.core.v1.ServiceAccount(
    "prometheus",
    metadata=k8s.meta.v1.ObjectMetaArgs(
        namespace=monitoring_ns.metadata.name,
    ),
    opts=pulumi.ResourceOptions(provider=k8s_provider, parent=monitoring_ns),
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
        "alertmanager": {
            "enabled": True,
            "alertmanagerSpec": {
                "tolerations": [
                    {
                        "key": "CriticalAddonsOnly",
                        "operator": "Equal",
                        "value": "true",
                        "effect": "NoSchedule",
                    },
                ],
            },
        },
        "kubeStateMetrics": {
            "tolerations": [
                {
                    "key": "CriticalAddonsOnly",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                },
            ],
        },
        "nodeExporter": {
            "tolerations": [
                {
                    "key": "CriticalAddonsOnly",
                    "operator": "Equal",
                    "value": "true",
                    "effect": "NoSchedule",
                },
            ],
        },
        "prometheus": {
            "ingress": {
                "enabled": True,
                "hosts": ["prometheus"],
                "ingressClassName": "tailscale",
                "tls": [
                    {
                        "hosts": ["prometheus"],
                    }
                ],
            },
            "serviceAccount": {
                "name": service_account.metadata.name,
                "create": False,
            },
            "prometheusSpec": {
                "externalLabels": {
                    "cluster": cluster.name,
                },
                "serviceMonitorSelector": {},
                "serviceMonitorSelectorNilUsesHelmValues": False,
                "tolerations": [
                    {
                        "key": "CriticalAddonsOnly",
                        "operator": "Equal",
                        "value": "true",
                        "effect": "NoSchedule",
                    },
                ],
            },
        },
    },
    opts=pulumi.ResourceOptions(parent=monitoring_ns, provider=k8s_provider),
)

metrics_svc = k8s.core.v1.Service(
    "kube-prometheus-ts",
    metadata=k8s.meta.v1.ObjectMetaArgs(
        namespace=monitoring_ns.metadata.name,
        name=f"prometheus-{STACK}",
        annotations={
            "pulumi.com/skipAwait": "true",
        }
    ),
    spec=k8s.core.v1.ServiceSpecArgs(
        type="LoadBalancer",
        load_balancer_class="tailscale",
        selector={
            "app.kubernetes.io/name": "prometheus",
            "operator.prometheus.io/name": pulumi.Output.concat(kube_prometheus.status.name, "-k-prometheus")
        },
        ports=[
            k8s.core.v1.ServicePortArgs(name="http-web", port=9090, target_port=9090),
            k8s.core.v1.ServicePortArgs(name="reloader-web", port=8080, target_port=8080),
        ],
    ),
    opts=pulumi.ResourceOptions(provider=k8s_provider, parent=kube_prometheus),
)

pulumi.export("kubelet_principal_id", cluster.kubelet_identity.object_id)
pulumi.export("node_resource_group", cluster.node_resource_group)
pulumi.export("kubeconfig", cluster.kube_config_raw)