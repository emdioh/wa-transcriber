//go:build linux

package netops

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func (o *linuxOps) StationCount(ifname string) (int, error) {
	iface, err := net.InterfaceByName(ifname)
	if err != nil {
		return 0, fmt.Errorf("netops: station count %q: %w", ifname, err)
	}

	fd, err := openNetlinkSocket(unix.NETLINK_GENERIC)
	if err != nil {
		return 0, fmt.Errorf("netops: station count %q: %w", ifname, err)
	}
	defer unix.Close(fd)

	familyID, err := o.resolveGenlFamily(fd, nl80211FamilyName)
	if err != nil {
		return 0, fmt.Errorf("netops: station count %q: resolve nl80211 family: %w", ifname, err)
	}

	seq := o.nextSeq()
	req := encodeNL80211GetStationDump(familyID, iface.Index, seq)
	if err := sendNetlink(fd, req); err != nil {
		return 0, fmt.Errorf("netops: station count %q: send GET_STATION: %w", ifname, err)
	}

	buf := make([]byte, 65536)
	total := 0
	for {
		n, err := recvNetlink(fd, buf)
		if err != nil {
			return 0, fmt.Errorf("netops: station count %q: recv GET_STATION: %w", ifname, err)
		}
		msgs, err := parseNLMessages(buf[:n])
		if err != nil {
			return 0, err
		}
		cnt, done, errno, err := parseNL80211StationDump(msgs, familyID, seq)
		if err != nil {
			return 0, err
		}
		total += cnt
		if errno != 0 {
			return 0, fmt.Errorf("netops: station count %q: %w", ifname, unix.Errno(errno))
		}
		if done {
			return total, nil
		}
	}
}

func (o *linuxOps) resolveGenlFamily(fd int, name string) (uint16, error) {
	seq := o.nextSeq()
	req := encodeGenlGetFamily(name, seq)
	if err := sendNetlink(fd, req); err != nil {
		return 0, err
	}

	buf := make([]byte, 8192)
	for {
		n, err := recvNetlink(fd, buf)
		if err != nil {
			return 0, err
		}
		msgs, err := parseNLMessages(buf[:n])
		if err != nil {
			return 0, err
		}
		id, found, errno, err := parseGenlFamilyID(msgs, seq)
		if err != nil {
			return 0, err
		}
		if found {
			if errno != 0 {
				return 0, unix.Errno(errno)
			}
			return id, nil
		}
	}
}
