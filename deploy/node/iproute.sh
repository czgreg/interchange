#!/bin/bash
# leap-gateway transparent proxy routing (TPROXY mode).
#
# Called by leap-nft.service on ExecStart (up) and ExecStop (down).
# Idempotent on both directions.
#
# What it sets up:
#   - iptables TPROXY rules: redirect non-DNS TCP/UDP from the FeiLian
#     client subnet on tun0 to mihomo's tproxy-port (7893). The kernel's
#     TPROXY target preserves the original client sourceIP (10.8.x.x) so
#     mihomo's /connections metadata shows real per-terminal IPs — unlike
#     TUN mode which collapses everything to 198.18.0.0.
#   - ip rule + ip route: route TPROXY-marked packets (fwmark 0x44) to
#     local (lo), so the kernel delivers them to mihomo's tproxy socket.
#
# Configuration is read from /etc/leap/env (written by install.sh):
#   LEAP_CLIENT_SUBNET   FeiLian client pool CIDR, e.g. 10.8.13.0/24
#   LEAP_TPROXY_PORT     mihomo tproxy-port, e.g. 7893
#   LEAP_TUN0_IFACE      FeiLian VPN interface, e.g. tun0

set -euo pipefail

ENV_FILE="/etc/leap/env"
[ -f "$ENV_FILE" ] && source "$ENV_FILE"

CLIENT_SUBNET="${LEAP_CLIENT_SUBNET:-}"
TPROXY_PORT="${LEAP_TPROXY_PORT:-0}"
API_PORT="${LEAP_API_PORT:-18080}"
TUN0_IFACE="${LEAP_TUN0_IFACE:-tun0}"
TPROXY_MARK="0x44"
TPROXY_TABLE="101"

[ -n "$CLIENT_SUBNET" ] || { echo "LEAP_CLIENT_SUBNET not set in $ENV_FILE" >&2; exit 1; }
# TPROXY is the production data path; tproxy_port=0 means gateway.yaml is
# misconfigured (TUN-only mode is no longer supported — collapses all client
# sourceIPs to 198.18.0.0). Fail fast rather than installing an iptables
# rule targeting port 0.
[ "$TPROXY_PORT" -gt 0 ] 2>/dev/null \
    || { echo "LEAP_TPROXY_PORT=$TPROXY_PORT — set data_plane.tproxy_port (e.g. 7893) in /etc/leap/gateway.yaml and re-run install.sh" >&2; exit 1; }
[ "$API_PORT" -gt 0 ] 2>/dev/null \
    || { echo "LEAP_API_PORT=$API_PORT — set api.listen to a valid port in /etc/leap/gateway.yaml and re-run install.sh" >&2; exit 1; }

tproxy_up() {
    # Ensure xt_TPROXY module is loaded.
    modprobe xt_TPROXY 2>/dev/null || true

    # Legacy cleanup: fwmark 0x42 / table 100 are sing-box-era leftovers
    # (its `tun.fwmark: 66 / routing_table: 100`). Nothing tags packets
    # with 0x42 anymore now that the sing-box engine is retired and
    # mihomo's TUN inbound has auto-route disabled — the rule + table
    # are inert but persist across boots until something cleans them.
    # Run on every up so a node deployed pre-2026-06 self-cleans.
    while ip rule del fwmark 0x42 lookup 100 2>/dev/null; do :; done
    ip route flush table 100 2>/dev/null || true

    # ip rule: TPROXY-marked packets → local routing table 101.
    while ip rule del fwmark "$TPROXY_MARK" lookup "$TPROXY_TABLE" 2>/dev/null; do :; done
    ip rule add fwmark "$TPROXY_MARK" lookup "$TPROXY_TABLE" pref 101

    # ip route: all destinations in table 101 go to loopback (local delivery).
    ip route flush table "$TPROXY_TABLE" 2>/dev/null || true
    ip route add local 0.0.0.0/0 dev lo table "$TPROXY_TABLE"

    # iptables TPROXY chain (idempotent: flush + re-add).
    iptables -t mangle -N LEAP_TPROXY 2>/dev/null || iptables -t mangle -F LEAP_TPROXY

    # Redirect non-DNS TCP from client subnet to mihomo tproxy-port.
    iptables -t mangle -A LEAP_TPROXY -p tcp \
        -j TPROXY --on-port "$TPROXY_PORT" --on-ip 127.0.0.1 \
        --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK"

    # Redirect non-DNS UDP from client subnet to mihomo tproxy-port.
    # Exclude DNS (port 53) — DNS still goes to mihomo's DNS server directly.
    iptables -t mangle -A LEAP_TPROXY -p udp ! --dport 53 \
        -j TPROXY --on-port "$TPROXY_PORT" --on-ip 127.0.0.1 \
        --tproxy-mark "$TPROXY_MARK/$TPROXY_MARK"

    # Block FeiLian client addresses from reaching the gateway API before
    # the generic TPROXY jump. Without this early drop, mihomo can accept a
    # client connection to the node's own API port before the inet/leap input
    # chain sees it, bypassing the source deny in nft.conf.
    while iptables -t mangle -D PREROUTING -i "$TUN0_IFACE" \
        -s 10.8.0.0/16 -m addrtype --dst-type LOCAL \
        -p tcp --dport "$API_PORT" -j DROP 2>/dev/null; do :; done
    iptables -t mangle -I PREROUTING 1 -i "$TUN0_IFACE" \
        -s 10.8.0.0/16 -m addrtype --dst-type LOCAL \
        -p tcp --dport "$API_PORT" -j DROP

    # Attach to PREROUTING: apply TPROXY only to client traffic on tun0.
    # Idempotent: delete existing rule before adding.
    iptables -t mangle -D PREROUTING -i "$TUN0_IFACE" -s "$CLIENT_SUBNET" \
        -j LEAP_TPROXY 2>/dev/null || true
    iptables -t mangle -A PREROUTING -i "$TUN0_IFACE" -s "$CLIENT_SUBNET" \
        -j LEAP_TPROXY

    echo "leap tproxy up: $CLIENT_SUBNET@$TUN0_IFACE -> 127.0.0.1:$TPROXY_PORT (fwmark $TPROXY_MARK)"
}

tproxy_down() {
    # Remove the API deny rule before removing the generic TPROXY jump.
    while iptables -t mangle -D PREROUTING -i "$TUN0_IFACE" \
        -s 10.8.0.0/16 -m addrtype --dst-type LOCAL \
        -p tcp --dport "$API_PORT" -j DROP 2>/dev/null; do :; done

    # Remove PREROUTING jump rule.
    iptables -t mangle -D PREROUTING -i "$TUN0_IFACE" -s "$CLIENT_SUBNET" \
        -j LEAP_TPROXY 2>/dev/null || true

    # Flush and remove the LEAP_TPROXY chain.
    iptables -t mangle -F LEAP_TPROXY 2>/dev/null || true
    iptables -t mangle -X LEAP_TPROXY 2>/dev/null || true

    # Remove ip rule and flush routing table.
    while ip rule del fwmark "$TPROXY_MARK" lookup "$TPROXY_TABLE" 2>/dev/null; do :; done
    ip route flush table "$TPROXY_TABLE" 2>/dev/null || true

    echo "leap tproxy down: $CLIENT_SUBNET cleared"
}

case "${1:-up}" in
  up)   tproxy_up ;;
  down) tproxy_down ;;
  *)    echo "usage: $0 {up|down}" >&2; exit 2 ;;
esac
