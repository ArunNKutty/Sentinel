#!/usr/bin/env bash
set -euo pipefail

TAP_DEV="ch-tap0"
TAP_IP="192.168.254.1"

echo "Setting up host network for Cloud Hypervisor..."

# 1. Create TAP device
if ! ip link show $TAP_DEV >/dev/null 2>&1; then
    sudo ip tuntap add name $TAP_DEV mode tap
    sudo ip addr add $TAP_IP/24 dev $TAP_DEV
    sudo ip link set $TAP_DEV up
fi

# 2. Enable IP forwarding
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null

# 3. Setup NAT
DEF_IF=$(ip route | grep default | awk 'NR==1 {print $5}')
if [[ -n "$DEF_IF" ]]; then
    # Masquerade traffic from TAP device out the default interface
    sudo iptables -t nat -C POSTROUTING -o $DEF_IF -j MASQUERADE 2>/dev/null || \
    sudo iptables -t nat -A POSTROUTING -o $DEF_IF -j MASQUERADE
    
    sudo iptables -C FORWARD -i $TAP_DEV -o $DEF_IF -j ACCEPT 2>/dev/null || \
    sudo iptables -A FORWARD -i $TAP_DEV -o $DEF_IF -j ACCEPT
    
    sudo iptables -C FORWARD -i $DEF_IF -o $TAP_DEV -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    sudo iptables -A FORWARD -i $DEF_IF -o $TAP_DEV -m state --state RELATED,ESTABLISHED -j ACCEPT
fi

echo "Host network setup complete on $TAP_DEV ($TAP_IP). NAT enabled via $DEF_IF."
