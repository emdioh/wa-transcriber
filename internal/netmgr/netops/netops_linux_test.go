//go:build linux

package netops

import (
	"testing"

	"golang.org/x/sys/unix"
)

// TestLinuxWireConstantsMatchUnix verifies that the platform-independent wire
// constants used in encoders/decoders match Linux UAPI values in x/sys/unix.
func TestLinuxWireConstantsMatchUnix(t *testing.T) {
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"SizeofNlMsghdr", nlmsgHdrLen, unix.SizeofNlMsghdr},
		{"SizeofIfInfomsg", ifInfoMsgLen, unix.SizeofIfInfomsg},
		{"SizeofIfAddrmsg", ifAddrMsgLen, unix.SizeofIfAddrmsg},
		{"SizeofRtMsg", rtMsgLen, unix.SizeofRtMsg},
		{"SizeofRtAttr", rtaHdrLen, unix.SizeofRtAttr},
		{"GENL_HDRLEN", genlHdrLen, unix.GENL_HDRLEN},

		{"NLMSG_NOOP", nlmsgNoop, unix.NLMSG_NOOP},
		{"NLMSG_ERROR", nlmsgError, unix.NLMSG_ERROR},
		{"NLMSG_DONE", nlmsgDone, unix.NLMSG_DONE},
		{"NLMSG_OVERRUN", nlmsgOverrun, unix.NLMSG_OVERRUN},

		{"NLM_F_REQUEST", nlmFRequest, unix.NLM_F_REQUEST},
		{"NLM_F_MULTI", nlmFMulti, unix.NLM_F_MULTI},
		{"NLM_F_ACK", nlmFAck, unix.NLM_F_ACK},
		{"NLM_F_DUMP", nlmFDump, unix.NLM_F_DUMP},
		{"NLM_F_REPLACE", nlmFReplace, unix.NLM_F_REPLACE},
		{"NLM_F_EXCL", nlmFExcl, unix.NLM_F_EXCL},
		{"NLM_F_CREATE", nlmFCreate, unix.NLM_F_CREATE},
		{"NLA_TYPE_MASK", nlaTypeMask, int(^uint16(unix.NLA_F_NESTED | unix.NLA_F_NET_BYTEORDER))},

		{"RTM_NEWLINK", rtmNewLink, unix.RTM_NEWLINK},
		{"RTM_NEWADDR", rtmNewAddr, unix.RTM_NEWADDR},
		{"RTM_DELADDR", rtmDelAddr, unix.RTM_DELADDR},
		{"RTM_NEWROUTE", rtmNewRoute, unix.RTM_NEWROUTE},

		{"AF_UNSPEC", afUnspec, unix.AF_UNSPEC},
		{"AF_INET", afInet, unix.AF_INET},
		{"IFF_UP", iffUp, unix.IFF_UP},

		{"IFA_ADDRESS", ifaAddress, unix.IFA_ADDRESS},
		{"IFA_LOCAL", ifaLocal, unix.IFA_LOCAL},
		{"IFA_BROADCAST", ifaBroadcast, unix.IFA_BROADCAST},

		{"RT_TABLE_MAIN", rtTableMain, unix.RT_TABLE_MAIN},
		{"RTPROT_BOOT", rtProtBoot, unix.RTPROT_BOOT},
		{"RT_SCOPE_UNIVERSE", rtScopeUniverse, unix.RT_SCOPE_UNIVERSE},
		{"RTN_UNICAST", rtNUnicast, unix.RTN_UNICAST},
		{"RTA_OIF", rtaOIF, unix.RTA_OIF},
		{"RTA_GATEWAY", rtaGateway, unix.RTA_GATEWAY},

		{"GENL_ID_CTRL", genlIDCtrl, unix.GENL_ID_CTRL},
		{"CTRL_CMD_NEWFAMILY", ctrlCmdNewFamily, unix.CTRL_CMD_NEWFAMILY},
		{"CTRL_CMD_GETFAMILY", ctrlCmdGetFamily, unix.CTRL_CMD_GETFAMILY},
		{"CTRL_ATTR_FAMILY_ID", ctrlAttrFamilyID, unix.CTRL_ATTR_FAMILY_ID},
		{"CTRL_ATTR_FAMILY_NAME", ctrlAttrFamilyNam, unix.CTRL_ATTR_FAMILY_NAME},

		{"ETH_P_ARP", int(ethProtoARP), unix.ETH_P_ARP},
		{"ETH_P_IP", int(ethProtoIPv4), unix.ETH_P_IP},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s: got %d (0x%x), want %d (0x%x)", c.name, c.got, c.got, c.want, c.want)
		}
	}
}
