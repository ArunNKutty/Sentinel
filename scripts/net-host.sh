#!/usr/bin/env bash
# Host-side network for the plasma fleet: the plasma-br0 bridge plus NAT so
# VMs on 10.0.0.0/24 can reach the outside world. plasmad creates the bridge
# and the per-VM taps on its own; what it does not do is forwarding and
# masquerading, which is what this script adds. Safe to re-run.
set -euo pipefail

BRIDGE="plasma-br0"
SUBNET="10.0.0.0/24"
GATEWAY="10.0.0.1/24"

echo "Setting up host network for the plasma fleet..."

# 1. Bridge (plasmad creates this too; doing it here keeps NAT valid before
#    the first VM launches)
if ! ip link show "$BRIDGE" >/dev/null 2>&1; then
    sudo ip link add name "$BRIDGE" type bridge
fi
if ! ip -4 addr show dev "$BRIDGE" | grep -q "${GATEWAY%/*}/"; then
    sudo ip addr add "$GATEWAY" dev "$BRIDGE"
fi
sudo ip link set "$BRIDGE" up

# 2. Enable IP forwarding
sudo sysctl -w net.ipv4.ip_forward=1 >/dev/null

# 3. NAT out the default interface
DEF_IF=$(ip route | awk '/^default/ {print $5; exit}')
if [[ -n "$DEF_IF" ]]; then
    sudo iptables -t nat -C POSTROUTING -s "$SUBNET" -o "$DEF_IF" -j MASQUERADE 2>/dev/null || \
    sudo iptables -t nat -A POSTROUTING -s "$SUBNET" -o "$DEF_IF" -j MASQUERADE

    sudo iptables -C FORWARD -i "$BRIDGE" -o "$DEF_IF" -j ACCEPT 2>/dev/null || \
    sudo iptables -A FORWARD -i "$BRIDGE" -o "$DEF_IF" -j ACCEPT

    sudo iptables -C FORWARD -i "$DEF_IF" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || \
    sudo iptables -A FORWARD -i "$DEF_IF" -o "$BRIDGE" -m state --state RELATED,ESTABLISHED -j ACCEPT
fi

# 4. Let VMs on the bridge talk to each other
sudo iptables -C FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT 2>/dev/null || \
sudo iptables -A FORWARD -i "$BRIDGE" -o "$BRIDGE" -j ACCEPT

echo "Host network ready: $BRIDGE (${GATEWAY%/*}), NAT for $SUBNET via ${DEF_IF:-<no default route>}."
