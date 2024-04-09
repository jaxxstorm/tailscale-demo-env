import ipaddress

def get_4via6_address(site_id, ipv4_cidr):
    # Fixed 64-bit prefix for Tailscale 4via6-routed packets
    fixed_prefix = "fd7a:115c:a1e0:b1a"
    
    # Validate site ID
    if site_id < 0 or site_id > 65535:
        raise ValueError("Site ID must be between 0 and 65535 (inclusive)")
    
    # Convert site ID to 32-bit translator identifier
    translator_id = f"0:{site_id}"
    
    # Parse the IPv4 CIDR block
    ipv4_network = ipaddress.IPv4Network(ipv4_cidr)
    
    # Get the network address of the IPv4 CIDR block
    ipv4_address = ipv4_network.network_address
    
    # Convert IPv4 address to 16-bit hex numbers
    ipv4_hex = f"{ipv4_address.packed[0]:02x}{ipv4_address.packed[1]:02x}:{ipv4_address.packed[2]:02x}{ipv4_address.packed[3]:02x}"
    
    # Get the prefix length of the IPv4 CIDR block
    prefix_length = ipv4_network.prefixlen
    
    # Construct the IPv6 address with the prefix length
    ipv6_address = f"{fixed_prefix}:{translator_id}:{ipv4_hex}/{prefix_length + 96}"
    
    return ipv6_address