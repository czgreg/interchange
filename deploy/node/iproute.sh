#!/bin/bash
# leap-gateway policy routing — fwmark 0x42 -> table 100 -> utun-leap
#
# Called by leap-nft.service. Idempotent on both up and down.

set -euo pipefail

TABLE="${LEAP_RT_TABLE:-100}"
MARK="${LEAP_FWMARK:-0x42}"
IFACE="${LEAP_TUN:-utun-leap}"

case "${1:-up}" in
  up)
    # Idempotent — clear any prior rule with the same selector before adding.
    while ip rule del fwmark "$MARK" lookup "$TABLE" 2>/dev/null; do :; done
    ip rule add fwmark "$MARK" lookup "$TABLE" pref 100

    # default route in our private table to utun-leap. sing-box TUN handles
    # what to do once the packet enters this interface.
    ip route flush table "$TABLE" 2>/dev/null || true
    ip route add default dev "$IFACE" table "$TABLE"

    echo "leap iproute up: fwmark $MARK -> table $TABLE -> dev $IFACE"
    ;;
  down)
    while ip rule del fwmark "$MARK" lookup "$TABLE" 2>/dev/null; do :; done
    ip route flush table "$TABLE" 2>/dev/null || true
    echo "leap iproute down: fwmark $MARK / table $TABLE cleared"
    ;;
  *)
    echo "usage: $0 {up|down}" >&2
    exit 2
    ;;
esac
