// The netlink functions this repository uses, each with the kernel
// deadline (one per function, same signature as package netlink).

package nlx

import (
	"net"

	"github.com/thxrben/cerium-switchd/lib/sys/hwio"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func LinkByName(name string) (netlink.Link, error) {
	return hwio.Do(Resource, "link by name"+desc(name), 0, func() (netlink.Link, error) { return netlink.LinkByName(name) })
}

func LinkList() ([]netlink.Link, error) {
	return hwio.Do(Resource, "link list"+desc(), 0, func() ([]netlink.Link, error) { return netlink.LinkList() })
}

func NeighList(linkIndex, family int) ([]netlink.Neigh, error) {
	return hwio.Do(Resource, "neigh list"+desc(), 0, func() ([]netlink.Neigh, error) { return netlink.NeighList(linkIndex, family) })
}

func LinkByIndex(index int) (netlink.Link, error) {
	return hwio.Do(Resource, "link by index"+desc(), 0, func() (netlink.Link, error) { return netlink.LinkByIndex(index) })
}

func LinkDel(link netlink.Link) error {
	return hwio.DoErr(Resource, "link del"+desc(link), 0, func() error { return netlink.LinkDel(link) })
}

func NeighDel(neigh *netlink.Neigh) error {
	return hwio.DoErr(Resource, "neigh del"+desc(), 0, func() error { return netlink.NeighDel(neigh) })
}

func LinkSetUp(link netlink.Link) error {
	return hwio.DoErr(Resource, "link set up"+desc(link), 0, func() error { return netlink.LinkSetUp(link) })
}

func LinkAdd(link netlink.Link) error {
	return hwio.DoErr(Resource, "link add"+desc(link), 0, func() error { return netlink.LinkAdd(link) })
}

func LinkSetNoMaster(link netlink.Link) error {
	return hwio.DoErr(Resource, "link set no master"+desc(link), 0, func() error { return netlink.LinkSetNoMaster(link) })
}

func LinkSetMaster(link netlink.Link, master netlink.Link) error {
	return hwio.DoErr(Resource, "link set master"+desc(link), 0, func() error { return netlink.LinkSetMaster(link, master) })
}

func AddrDel(link netlink.Link, addr *netlink.Addr) error {
	return hwio.DoErr(Resource, "addr del"+desc(link), 0, func() error { return netlink.AddrDel(link, addr) })
}

func RouteReplace(route *netlink.Route) error {
	return hwio.DoErr(Resource, "route replace"+desc(), 0, func() error { return netlink.RouteReplace(route) })
}

func NeighSet(neigh *netlink.Neigh) error {
	return hwio.DoErr(Resource, "neigh set"+desc(), 0, func() error { return netlink.NeighSet(neigh) })
}

func AddrList(link netlink.Link, family int) ([]netlink.Addr, error) {
	return hwio.Do(Resource, "addr list"+desc(link), 0, func() ([]netlink.Addr, error) { return netlink.AddrList(link, family) })
}

func AddrAdd(link netlink.Link, addr *netlink.Addr) error {
	return hwio.DoErr(Resource, "addr add"+desc(link), 0, func() error { return netlink.AddrAdd(link, addr) })
}

func RouteListFiltered(family int, filter *netlink.Route, filterMask uint64) ([]netlink.Route, error) {
	return hwio.Do(Resource, "route list filtered"+desc(), 0, func() ([]netlink.Route, error) { return netlink.RouteListFiltered(family, filter, filterMask) })
}

func LinkSetMTU(link netlink.Link, mtu int) error {
	return hwio.DoErr(Resource, "link set mtu"+desc(link), 0, func() error { return netlink.LinkSetMTU(link, mtu) })
}

func LinkSetDown(link netlink.Link) error {
	return hwio.DoErr(Resource, "link set down"+desc(link), 0, func() error { return netlink.LinkSetDown(link) })
}

func FilterList(link netlink.Link, parent uint32) ([]netlink.Filter, error) {
	return hwio.Do(Resource, "filter list"+desc(link), 0, func() ([]netlink.Filter, error) { return netlink.FilterList(link, parent) })
}

func RouteDel(route *netlink.Route) error {
	return hwio.DoErr(Resource, "route del"+desc(), 0, func() error { return netlink.RouteDel(route) })
}

func LinkSetLearning(link netlink.Link, mode bool) error {
	return hwio.DoErr(Resource, "link set learning"+desc(link), 0, func() error { return netlink.LinkSetLearning(link, mode) })
}

func LinkSetAlias(link netlink.Link, name string) error {
	return hwio.DoErr(Resource, "link set alias"+desc(link), 0, func() error { return netlink.LinkSetAlias(link, name) })
}

func LinkModify(link netlink.Link) error {
	return hwio.DoErr(Resource, "link modify"+desc(link), 0, func() error { return netlink.LinkModify(link) })
}

func BridgeVlanList() (map[int32][]*nl.BridgeVlanInfo, error) {
	return hwio.Do(Resource, "bridge vlan list"+desc(), 0, func() (map[int32][]*nl.BridgeVlanInfo, error) { return netlink.BridgeVlanList() })
}

func BridgeVlanDel(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return hwio.DoErr(Resource, "bridge vlan del"+desc(link), 0, func() error { return netlink.BridgeVlanDel(link, vid, pvid, untagged, self, master) })
}

func BridgeVlanAdd(link netlink.Link, vid uint16, pvid, untagged, self, master bool) error {
	return hwio.DoErr(Resource, "bridge vlan add"+desc(link), 0, func() error { return netlink.BridgeVlanAdd(link, vid, pvid, untagged, self, master) })
}

func RuleList(family int) ([]netlink.Rule, error) {
	return hwio.Do(Resource, "rule list"+desc(), 0, func() ([]netlink.Rule, error) { return netlink.RuleList(family) })
}

func RuleDel(rule *netlink.Rule) error {
	return hwio.DoErr(Resource, "rule del"+desc(), 0, func() error { return netlink.RuleDel(rule) })
}

func RuleAdd(rule *netlink.Rule) error {
	return hwio.DoErr(Resource, "rule add"+desc(), 0, func() error { return netlink.RuleAdd(rule) })
}

func RouteGetWithOptions(destination net.IP, options *netlink.RouteGetOptions) ([]netlink.Route, error) {
	return hwio.Do(Resource, "route get with options"+desc(), 0, func() ([]netlink.Route, error) { return netlink.RouteGetWithOptions(destination, options) })
}

func QdiscAdd(qdisc netlink.Qdisc) error {
	return hwio.DoErr(Resource, "qdisc add"+desc(), 0, func() error { return netlink.QdiscAdd(qdisc) })
}

func NeighAppend(neigh *netlink.Neigh) error {
	return hwio.DoErr(Resource, "neigh append"+desc(), 0, func() error { return netlink.NeighAppend(neigh) })
}

func LinkSetIsolated(link netlink.Link, mode bool) error {
	return hwio.DoErr(Resource, "link set isolated"+desc(link), 0, func() error { return netlink.LinkSetIsolated(link, mode) })
}

func LinkSetHardwareAddr(link netlink.Link, hwaddr net.HardwareAddr) error {
	return hwio.DoErr(Resource, "link set hardware addr"+desc(link), 0, func() error { return netlink.LinkSetHardwareAddr(link, hwaddr) })
}

func LinkGetProtinfo(link netlink.Link) (netlink.Protinfo, error) {
	return hwio.Do(Resource, "link get protinfo"+desc(link), 0, func() (netlink.Protinfo, error) { return netlink.LinkGetProtinfo(link) })
}

func GenlFamilyGet(name string) (*netlink.GenlFamily, error) {
	return hwio.Do(Resource, "genl family get"+desc(name), 0, func() (*netlink.GenlFamily, error) { return netlink.GenlFamilyGet(name) })
}
