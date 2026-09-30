-- Wireshark dissectors for switchd's own protocols (docs/stack-protocol.md).
--
--   swstack  the stacking link: EtherType 0x88b5 frames on stacking ports
--            (HELLO, DATA, ACK, RESET, BFD), with the byte stream of DATA
--            frames reassembled: the session mode byte, then TLS 1.3
--   swmesh   inside the member sessions' TLS (ALPN swstack/1): the
--            neighbour hello and the mesh messages (LSA, OPEN, DATA,
--            CREDIT, CLOSE, RESET); stream data of the RPC services ("ctl",
--            "cli") as JSON lines, of "raft" as hashicorp/raft msgpack
--   swjoin   inside a join session's TLS (ALPN swjoin/1): JSON lines
--   stack tunnels: VXLAN between the members' internal addresses
--            (169.254.64.<member>) is annotated with the members
--
-- The TLS layer is decrypted when Wireshark has the session keys: start
-- switchd with SWITCHD_TLS_KEYLOG=<file> and point Wireshark's TLS
-- preference "(Pre)-Master-Secret log filename" at that file (see
-- tools/wireshark/README.md). LACP and VXLAN themselves are standard and
-- decoded by Wireshark's built-in dissectors.

local ETHERTYPE = 0x88b5
local MAGIC = 0x5354

-------------------------------------------------------------------------------
-- swstack: the stacking link
-------------------------------------------------------------------------------

local swstack = Proto("swstack", "switchd stacking link")

local link_types = { [1] = "HELLO", [2] = "DATA", [3] = "ACK", [4] = "RESET", [5] = "BFD", [6] = "PROBE", [7] = "PROBE-REPLY" }

local f = swstack.fields
f.magic      = ProtoField.uint16("swstack.magic", "Magic", base.HEX)
f.version    = ProtoField.uint8("swstack.version", "Version")
f.type       = ProtoField.uint8("swstack.type", "Type", base.DEC, link_types)
f.epoch      = ProtoField.uint32("swstack.epoch", "Sender epoch", base.HEX)
f.peer_epoch = ProtoField.uint32("swstack.peer_epoch", "Receiver epoch (as seen)", base.HEX)
f.seq        = ProtoField.uint32("swstack.seq", "Sequence number")
f.ack        = ProtoField.uint32("swstack.ack", "Acknowledgement (next byte expected)")
f.window     = ProtoField.uint32("swstack.window", "Receive window (bytes)")
f.len        = ProtoField.uint16("swstack.len", "Payload length")
f.nextseq    = ProtoField.uint32("swstack.nextseq", "Next sequence number")
f.interval   = ProtoField.uint16("swstack.bfd.interval", "BFD interval (ms)")
f.multiplier = ProtoField.uint16("swstack.bfd.multiplier", "BFD multiplier")
f.payload    = ProtoField.bytes("swstack.payload", "Payload")
f.mode       = ProtoField.string("swstack.mode", "Session mode")
f.stream_off = ProtoField.uint32("swstack.stream_offset", "Stream offset")
f.probe_size = ProtoField.uint32("swstack.probe_size", "Probe size (Ethernet payload bytes)")

local e_retrans = ProtoExpert.new("swstack.retransmission", "Retransmission (bytes already seen)",
	expert.group.SEQUENCE, expert.severity.NOTE)
local e_gap = ProtoExpert.new("swstack.gap", "Gap: earlier bytes missing (lost, retransmitted later)",
	expert.group.SEQUENCE, expert.severity.WARN)
local e_reset = ProtoExpert.new("swstack.reset", "Link reset", expert.group.SEQUENCE, expert.severity.WARN)
swstack.experts = { e_retrans, e_gap, e_reset }

-- Stream reassembly (first pass): per direction and sender epoch, the bytes
-- that arrived in order; complete units (the mode byte, TLS records) are
-- remembered per frame number and dissected from there on every pass.
local streams = {}  -- key -> { next = stream offset expected, buf = ByteArray (unparsed), started = bool }
local units = {}    -- frame number -> { { kind, bytes } ... }
local marks = {}    -- frame number -> "retrans" | "gap"

function swstack.init()
	streams, units, marks = {}, {}, {}
end

local function rest(b, from)
	if from >= b:len() then return ByteArray.new() end
	return b:subset(from, b:len() - from)
end

local function parse_units(st, out)
	while true do
		local b = st.buf
		if not st.started then
			if b:len() < 1 then return end
			out[#out + 1] = { "mode", b:subset(0, 1) }
			st.buf = rest(b, 1)
			st.started = true
		else
			if b:len() < 5 then return end
			local n = 5 + b:get_index(3) * 256 + b:get_index(4)
			if b:len() < n then return end
			out[#out + 1] = { "tls", b:subset(0, n) }
			st.buf = rest(b, n)
		end
	end
end

local tls = Dissector.get("tls")

function swstack.dissector(tvb, pinfo, tree)
	if tvb:len() < 24 or tvb(0, 2):uint() ~= MAGIC then return 0 end
	pinfo.cols.protocol = "SWSTACK"
	local typ = tvb(3, 1):uint()
	local seq, n = tvb(12, 4):uint(), tvb(22, 2):uint()
	local t = tree:add(swstack, tvb(0, 24 + math.min(n, tvb:len() - 24)))
	t:add(f.magic, tvb(0, 2))
	t:add(f.version, tvb(2, 1))
	t:add(f.type, tvb(3, 1))
	t:add(f.epoch, tvb(4, 4))
	t:add(f.peer_epoch, tvb(8, 4))
	t:add(f.seq, tvb(12, 4))
	t:add(f.ack, tvb(16, 4))
	t:add(f.window, tvb(20, 2), tvb(20, 2):uint() * 64)
	t:add(f.len, tvb(22, 2))
	local name = link_types[typ] or ("type " .. typ)
	local info = string.format("%s epoch %08x seq=%u ack=%u win=%u", name, tvb(4, 4):uint(), seq, tvb(16, 4):uint(), tvb(20, 2):uint() * 64)
	if n > 0 then info = info .. " len=" .. n end
	pinfo.cols.info = info

	if typ == 1 and n >= 4 then
		t:add(f.interval, tvb(24, 2))
		t:add(f.multiplier, tvb(26, 2))
	elseif typ == 4 then
		t:add_proto_expert_info(e_reset)
	elseif typ == 6 then
		-- Path MTU probe: padded to the size under test.
		t:add(f.probe_size, 24 + n):set_generated()
		pinfo.cols.info = string.format("PROBE %d bytes (path MTU test)", 24 + n)
		return tvb:len()
	elseif typ == 7 and n >= 4 then
		t:add(f.probe_size, tvb(24, 4))
		pinfo.cols.info = string.format("PROBE-REPLY: %d bytes arrived", tvb(24, 4):uint())
		return tvb:len()
	end
	if typ ~= 2 or n == 0 then
		if n > 0 and typ ~= 1 then t:add(f.payload, tvb(24, n)) end
		return tvb:len()
	end

	t:add(f.nextseq, seq + n):set_generated()
	t:add(f.payload, tvb(24, n))
	if not pinfo.visited then
		local key = tostring(pinfo.dl_src) .. ">" .. tostring(pinfo.dl_dst) .. "/" .. tvb(4, 4):uint()
		local st = streams[key]
		if not st then
			-- A capture that starts mid-stream cannot be aligned to TLS
			-- records: such a stream is only annotated.
			st = { next = seq, buf = ByteArray.new(), started = false, mid = seq ~= 0 }
			streams[key] = st
		end
		if st.mid then
			if seq >= st.next then st.next = seq + n else marks[pinfo.number] = "retrans" end
		elseif seq == st.next then
			st.buf:append(tvb(24, n):bytes())
			st.next = seq + n
			local out = {}
			parse_units(st, out)
			units[pinfo.number] = out
		elseif seq < st.next then
			marks[pinfo.number] = "retrans"
		else
			marks[pinfo.number] = "gap"
		end
	end
	t:add(f.stream_off, seq):set_generated()
	if marks[pinfo.number] == "retrans" then
		t:add_proto_expert_info(e_retrans)
		pinfo.cols.info:append(" [retransmission]")
	elseif marks[pinfo.number] == "gap" then
		t:add_proto_expert_info(e_gap)
		pinfo.cols.info:append(" [gap]")
	end
	-- All TLS records that complete in this frame go to the TLS dissector as
	-- one buffer: it keeps its per-record state by frame number and offset.
	local records = ByteArray.new()
	for _, u in ipairs(units[pinfo.number] or {}) do
		if u[1] == "mode" then
			local sub = u[2]:tvb("Session mode")
			local m = sub(0, 1):string()
			local what = ({ M = "member session", J = "joining with a token" })[m] or "?"
			tree:add(f.mode, sub(0, 1), m .. " (" .. what .. ")")
			pinfo.cols.info:append(" [mode " .. m .. "]")
		else
			records:append(u[2])
		end
	end
	if records:len() > 0 and tls then
		tls:call(records:tvb("Stacking stream"), pinfo, tree)
	end
	return tvb:len()
end

DissectorTable.get("ethertype"):add(ETHERTYPE, swstack)

-------------------------------------------------------------------------------
-- swmesh: mesh messages inside a member session
-------------------------------------------------------------------------------

local swmesh = Proto("swmesh", "switchd stack mesh")

local mesh_types = { [1] = "LSA", [2] = "OPEN", [3] = "DATA", [4] = "CREDIT", [5] = "CLOSE", [6] = "RESET" }

local m = swmesh.fields
m.len       = ProtoField.uint32("swmesh.len", "Length")
m.type      = ProtoField.uint8("swmesh.type", "Type", base.DEC, mesh_types)
m.hops      = ProtoField.uint8("swmesh.hops", "Hop limit")
m.src       = ProtoField.uint8("swmesh.src", "Source member")
m.dst       = ProtoField.uint8("swmesh.dst", "Destination member")
m.stream    = ProtoField.uint32("swmesh.stream", "Stream id")
m.seq       = ProtoField.uint32("swmesh.seq", "Sequence number")
m.credit    = ProtoField.uint32("swmesh.credit", "Bytes granted")
m.lsa_seq   = ProtoField.uint64("swmesh.lsa.seq", "LSA sequence number")
m.neighbor  = ProtoField.uint8("swmesh.lsa.neighbor", "Neighbour")
m.service   = ProtoField.string("swmesh.service", "Service")
m.data      = ProtoField.bytes("swmesh.data", "Stream data")
m.text      = ProtoField.string("swmesh.text", "Text")
m.hello     = ProtoField.string("swmesh.hello", "Neighbour hello")
m.raft_type = ProtoField.uint8("swmesh.raft.rpc", "hashicorp/raft RPC", base.DEC,
	{ [0] = "AppendEntries", [1] = "RequestVote", [2] = "InstallSnapshot", [3] = "TimeoutNow", [4] = "AppendEntriesPipeline" })

local services = {}  -- "a-b/stream" (a < b) -> service name, from OPEN
function swmesh.init() services = {} end

local json = Dissector.get("json")

local function service_key(a, b, s)
	if a > b then a, b = b, a end
	return a .. "-" .. b .. "/" .. s
end

local function add_lines(tvb, pinfo, tree)
	-- JSON lines (RPC and join messages): each complete line as JSON.
	local off, len = 0, tvb:len()
	while off < len do
		local nl = off
		while nl < len and tvb(nl, 1):uint() ~= 0x0a do nl = nl + 1 end
		local l = nl - off
		if l > 0 then
			if json and tvb(off, 1):uint() == 0x7b and nl < len then
				json:call(tvb(off, l):tvb(), pinfo, tree)
			else
				tree:add(m.text, tvb(off, l))
			end
		end
		off = nl + 1
	end
end

local function dissect_msg(tvb, pinfo, tree)
	local typ, src, dst = tvb(4, 1):uint(), tvb(6, 1):uint(), tvb(7, 1):uint()
	local stream, seq = tvb(8, 4):uint(), tvb(12, 4):uint()
	local name = mesh_types[typ] or ("type " .. typ)
	local t = tree:add(swmesh, tvb, name)
	t:add(m.len, tvb(0, 4))
	t:add(m.type, tvb(4, 1))
	t:add(m.hops, tvb(5, 1))
	t:add(m.src, tvb(6, 1))
	t:add(m.dst, tvb(7, 1))
	local p = tvb(16):len() > 0 and tvb(16) or nil
	local key = service_key(src, dst, stream)
	local summary
	if typ == 1 then
		local nb = {}
		if p and p:len() >= 8 then
			t:add(m.lsa_seq, p(0, 8))
			for i = 8, p:len() - 1 do
				t:add(m.neighbor, p(i, 1))
				nb[#nb + 1] = tostring(p(i, 1):uint())
			end
		end
		summary = string.format("LSA from %d: neighbours [%s]", src, table.concat(nb, " "))
		t:append_text(string.format(", origin %d", src))
		return summary
	end
	t:add(m.stream, tvb(8, 4))
	if typ == 4 then
		t:add(m.credit, tvb(12, 4))
	else
		t:add(m.seq, tvb(12, 4))
	end
	summary = string.format("%s %d→%d s%u", name, src, dst, stream)
	if typ == 2 and p then
		local svc = p:string()
		t:add(m.service, p)
		if not pinfo.visited then services[key] = svc end
		summary = summary .. " " .. svc
	elseif typ == 3 and p then
		local svc = services[key]
		t:add(m.service, svc or "(stream opened before the capture)"):set_generated()
		t:add(m.data, p)
		summary = summary .. string.format(" %s %d bytes", svc or "?", p:len())
		if svc == "ctl" or svc == "cli" then
			add_lines(p:tvb(), pinfo, t)
		elseif svc == "raft" and p:len() > 1 and p(0, 1):uint() <= 4 then
			t:add(m.raft_type, p(0, 1)):append_text(" (if this starts a request; msgpack follows)")
		end
	elseif typ == 4 then
		summary = summary .. string.format(" +%u bytes", seq)
	elseif p then
		t:add(m.text, p)
	end
	t:append_text(", " .. summary)
	return summary
end

function swmesh.dissector(tvb, pinfo, tree)
	pinfo.cols.protocol = "SWMESH"
	local off, len = 0, tvb:len()
	local infos = {}
	while off < len do
		if tvb(off, 1):uint() == 0x7b then
			-- The neighbour hello: one JSON line after the handshake.
			local nl = off
			while nl < len and tvb(nl, 1):uint() ~= 0x0a do nl = nl + 1 end
			if nl == len then
				pinfo.desegment_offset, pinfo.desegment_len = off, DESEGMENT_ONE_MORE_SEGMENT
				return len
			end
			local t = tree:add(swmesh, tvb(off, nl - off + 1), "Neighbour hello")
			t:add(m.hello, tvb(off, nl - off))
			if json then json:call(tvb(off, nl - off):tvb(), pinfo, t) end
			infos[#infos + 1] = "hello " .. tvb(off, nl - off):string()
			off = nl + 1
		else
			if len - off < 4 then
				pinfo.desegment_offset, pinfo.desegment_len = off, DESEGMENT_ONE_MORE_SEGMENT
				return len
			end
			local n = tvb(off, 4):uint()
			if n < 12 or n > 40000 then
				tree:add(m.data, tvb(off)):append_text(" (not a mesh message)")
				break
			end
			if len - off < 4 + n then
				pinfo.desegment_offset, pinfo.desegment_len = off, 4 + n - (len - off)
				return len
			end
			infos[#infos + 1] = dissect_msg(tvb(off, 4 + n):tvb(), pinfo, tree)
			off = off + 4 + n
		end
	end
	if #infos > 0 then pinfo.cols.info = table.concat(infos, "; ") end
	return len
end

-------------------------------------------------------------------------------
-- swjoin: the join exchange (JSON lines)
-------------------------------------------------------------------------------

local swjoin = Proto("swjoin", "switchd stack join")

function swjoin.dissector(tvb, pinfo, tree)
	pinfo.cols.protocol = "SWJOIN"
	local t = tree:add(swjoin, tvb)
	add_lines(tvb, pinfo, t)
	pinfo.cols.info = "join exchange"
	return tvb:len()
end

local alpn = DissectorTable.get("tls.alpn")
if alpn then
	alpn:add("swstack/1", swmesh)
	alpn:add("swjoin/1", swjoin)
end

-------------------------------------------------------------------------------
-- Stack tunnels: VXLAN between members (annotation)
-------------------------------------------------------------------------------

local swtunnel = Proto("swtunnel", "switchd stack tunnel")
local tu = swtunnel.fields
tu.from = ProtoField.uint8("swtunnel.from", "From member")
tu.to   = ProtoField.uint8("swtunnel.to", "To member")

local ip_src, ip_dst = Field.new("ip.src"), Field.new("ip.dst")
local vxlan_vni = Field.new("vxlan.vni")

local function member_of(a)
	local s = tostring(a)
	local x = s:match("^169%.254%.64%.(%d+)$")
	return x and tonumber(x)
end

function swtunnel.dissector(tvb, pinfo, tree)
	local vni = vxlan_vni()
	if not vni then return end
	local srcs, dsts = { ip_src() }, { ip_dst() }
	if #srcs == 0 then return end
	local a, b = member_of(srcs[1].value), member_of(dsts[1].value)
	if not a or not b then return end
	local t = tree:add(swtunnel, string.format("switchd stack tunnel: member %d → member %d (VNI %d)", a, b, vni.value))
	t:add(tu.from, a):set_generated()
	t:add(tu.to, b):set_generated()
end

register_postdissector(swtunnel)
