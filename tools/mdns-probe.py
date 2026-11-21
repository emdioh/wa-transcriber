#!/usr/bin/env python3
"""Resolve a name the way Android 12+ does and report what came back.

Android's DNS resolver gained one-shot mDNS queries (RFC 6762 section 5.1)
with the Google Play System Update of November 2021; older Android cannot
resolve .local at all. A one-shot query comes from an ephemeral port with the
unicast-response (QU) bit set and expects a *conventional* unicast answer:
same query ID, the question repeated, no cache-flush bit, TTL <= 10 s
(RFC 6762 section 6.7). This script sends exactly that and prints every reply,
so "the phone cannot find wa.local" becomes a triangle between the device, any
mDNS reflector on the LAN, and the phone.

Usage:
    ./mdns-probe.py                      # wa.local via multicast
    ./mdns-probe.py --name wa.local      # same, explicit
    ./mdns-probe.py --unicast 192.168.15.43   # ask the device directly
    ./mdns-probe.py --source 192.168.15.159   # pick the outgoing interface

A reply from the device is the one with the cache-flush bit absent and TTL 10;
a reflector (routers, Home Assistant, OpenWrt with avahi) usually answers from
its cache with TTL 120 and no question section.
"""
import argparse
import os
import socket
import struct
import sys
import time

GROUP = ("224.0.0.251", 5353)


def encode_name(name):
    out = b""
    for label in name.strip(".").split("."):
        out += bytes([len(label)]) + label.encode()
    return out + b"\x00"


def decode_name(data, off):
    parts = []
    while True:
        length = data[off]
        if length == 0:
            return ".".join(parts), off + 1
        if length & 0xC0:
            return ".".join(parts), off + 2
        parts.append(data[off + 1:off + 1 + length].decode("utf-8", "replace"))
        off += 1 + length


def parse(data):
    qid, flags, qd, an, ns, ar = struct.unpack("!HHHHHH", data[:12])
    off = 12
    questions = []
    for _ in range(qd):
        name, off = decode_name(data, off)
        qtype, qclass = struct.unpack("!HH", data[off:off + 4])
        off += 4
        questions.append(f"{name} type{qtype} class=0x{qclass:04x}")
    answers = []
    for _ in range(an):
        name, off = decode_name(data, off)
        rtype, rclass, ttl, rdlen = struct.unpack("!HHIH", data[off:off + 10])
        off += 10
        rdata = data[off:off + rdlen]
        off += rdlen
        if rtype == 1 and rdlen == 4:
            value = socket.inet_ntoa(rdata)
        elif rtype == 28 and rdlen == 16:
            value = socket.inet_ntop(socket.AF_INET6, rdata)
        else:
            value = rdata.hex()
        answers.append(f"{name} type{rtype} class=0x{rclass:04x} ttl={ttl} {value}")
    return qid, questions, answers


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--name", default="wa.local")
    ap.add_argument("--unicast", metavar="IP",
                    help="send the query straight to this address instead of the group")
    ap.add_argument("--source", metavar="IP", help="outgoing IPv4 interface")
    ap.add_argument("--wait", type=float, default=3.0, help="seconds to collect replies")
    args = ap.parse_args()

    # Any non-zero identifier will do; it only has to come back unchanged.
    qid = int.from_bytes(os.urandom(2), "big") or 1
    pkt = struct.pack("!HHHHHH", qid, 0, 1, 0, 0, 0)
    pkt += encode_name(args.name) + struct.pack("!HH", 1, 0x8001)  # A + QU bit

    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(args.wait)
    if args.unicast:
        s.sendto(pkt, (args.unicast, 5353))
        where = f"unicast to {args.unicast}:5353"
    else:
        if args.source:
            s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_IF,
                         socket.inet_aton(args.source))
        s.setsockopt(socket.IPPROTO_IP, socket.IP_MULTICAST_TTL, 255)
        s.sendto(pkt, GROUP)
        where = f"multicast to {GROUP[0]}:{GROUP[1]}"
    print(f"query {args.name} (id={qid:#06x}) {where}, from local port {s.getsockname()[1]}")

    seen, end = 0, time.time() + args.wait
    while time.time() < end:
        try:
            data, src = s.recvfrom(4096)
        except TimeoutError:
            break
        rid, questions, answers = parse(data)
        seen += 1
        ok = "ID OK" if rid == qid else f"ID {rid:#06x} MISMATCH (a resolver may drop this)"
        print(f"  reply from {src[0]}:{src[1]}: {ok}")
        print(f"    question: {questions or '(none - not a conventional reply)'}")
        for a in answers:
            print(f"    answer:   {a}")
    s.close()
    if not seen:
        print("  no reply")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
