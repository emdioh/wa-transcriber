package netops

import (
	"encoding/binary"
	"fmt"
)

// Generic Netlink and nl80211 constants (linux/genetlink.h, linux/nl80211.h).
const (
	genlHdrLen = 4

	genlIDCtrl        = 0x10 // GENL_ID_CTRL (NLMSG_MIN_TYPE)
	ctrlCmdNewFamily  = 1
	ctrlCmdGetFamily  = 3
	ctrlAttrFamilyID  = 1
	ctrlAttrFamilyNam = 2

	nl80211FamilyName    = "nl80211"
	nl80211CmdGetStation = 17
	nl80211CmdNewStation = 19
	nl80211AttrIfindex   = 3
)

// encodeGenlGetFamily encodes a CTRL_CMD_GETFAMILY request for familyName.
func encodeGenlGetFamily(familyName string, seq uint32) []byte {
	b := newNLBuilder(genlIDCtrl, nlmFRequest, seq, genlHdrLen)
	body := b.body()
	body[0] = ctrlCmdGetFamily
	body[1] = 1 // version
	// body[2:4] = reserved (0)
	b.addAttrString(ctrlAttrFamilyNam, familyName)
	return b.bytes()
}

// parseGenlFamilyID scans Netlink messages for a CTRL_CMD_NEWFAMILY response
// matching seq and returns its CTRL_ATTR_FAMILY_ID.
// If an NLMSG_ERROR with non-zero errno is encountered, errno > 0 is returned.
func parseGenlFamilyID(msgs []nlMsg, seq uint32) (familyID uint16, found bool, errno int, err error) {
	for _, m := range msgs {
		if m.Header.Seq != seq {
			continue
		}
		if m.Header.Type == nlmsgNoop {
			continue
		}
		if m.Header.Type == nlmsgError {
			code, err := parseNLMsgError(m.Data)
			if err != nil {
				return 0, false, 0, err
			}
			if code != 0 {
				return 0, true, code, nil
			}
			continue
		}
		if m.Header.Type != genlIDCtrl {
			continue
		}
		if len(m.Data) < genlHdrLen {
			return 0, false, 0, fmt.Errorf("netops: truncated genlmsghdr (%d bytes)", len(m.Data))
		}
		if m.Data[0] != ctrlCmdNewFamily {
			continue
		}
		attrs, err := parseNLAttrs(m.Data[genlHdrLen:])
		if err != nil {
			return 0, false, 0, err
		}
		for _, a := range attrs {
			if a.Type == ctrlAttrFamilyID {
				if len(a.Value) != 2 {
					return 0, false, 0, fmt.Errorf("netops: invalid CTRL_ATTR_FAMILY_ID length %d", len(a.Value))
				}
				return binary.NativeEndian.Uint16(a.Value), true, 0, nil
			}
		}
		return 0, false, 0, fmt.Errorf("netops: CTRL_CMD_NEWFAMILY missing CTRL_ATTR_FAMILY_ID")
	}
	return 0, false, 0, nil
}

// encodeNL80211GetStationDump encodes an NL80211_CMD_GET_STATION dump request
// for ifindex using the resolved nl80211 generic netlink familyID.
func encodeNL80211GetStationDump(familyID uint16, ifindex int, seq uint32) []byte {
	b := newNLBuilder(familyID, nlmFRequest|nlmFDump, seq, genlHdrLen)
	body := b.body()
	body[0] = nl80211CmdGetStation
	body[1] = 0 // version
	// body[2:4] = reserved (0)
	b.addAttrUint32(nl80211AttrIfindex, uint32(ifindex))
	return b.bytes()
}

// parseNL80211StationDump counts NL80211_CMD_NEW_STATION messages matching
// familyID and seq, and reports whether NLMSG_DONE (or a non-multipart reply)
// terminated the dump. If NLMSG_ERROR (or NLMSG_DONE error payload) carries a
// non-zero errno, errno > 0 is returned.
func parseNL80211StationDump(msgs []nlMsg, familyID uint16, seq uint32) (count int, done bool, errno int, err error) {
	for _, m := range msgs {
		if m.Header.Seq != seq {
			continue
		}
		switch m.Header.Type {
		case nlmsgNoop:
			continue
		case nlmsgError:
			code, err := parseNLMsgError(m.Data)
			if err != nil {
				return count, true, 0, err
			}
			return count, true, code, nil
		case nlmsgDone:
			if len(m.Data) >= nlmsgErrLen {
				code, err := parseNLMsgError(m.Data)
				if err != nil {
					return count, true, 0, err
				}
				if code != 0 {
					return count, true, code, nil
				}
			}
			return count, true, 0, nil
		case familyID:
			if len(m.Data) < genlHdrLen {
				return count, false, 0, fmt.Errorf("netops: truncated nl80211 genlmsghdr (%d bytes)", len(m.Data))
			}
			if m.Data[0] == nl80211CmdNewStation {
				count++
			}
			if m.Header.Flags&nlmFMulti == 0 {
				return count, true, 0, nil
			}
		}
	}
	return count, false, 0, nil
}
