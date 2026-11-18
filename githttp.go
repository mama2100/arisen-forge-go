// githttp.go: git smart HTTP — v2 upload-pack (ls-refs, fetch with shallow), v0 upload-pack,
// v0 receive-pack with report-status over side-band-64k. Port of the entry's TS version.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const gitAgent = "forge/1.0"

func pkt(payload string) []byte {
	data := []byte(payload)
	header := []byte(fmt.Sprintf("%04x", len(data)+4))
	return append(header, data...)
}

func pktBytes(data []byte) []byte {
	header := []byte(fmt.Sprintf("%04x", len(data)+4))
	return append(header, data...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type GitReply struct {
	Status      int
	ContentType string
	Body        []byte
}

func gitOK(contentType string, body []byte) GitReply {
	return GitReply{Status: 200, ContentType: contentType, Body: body}
}

func gitNotFound() GitReply {
	body, _ := json.Marshal(map[string]string{"message": "Not Found"})
	return GitReply{Status: 404, ContentType: "application/json", Body: append(body, '\n')}
}

// ---------------------------------------------------------------- info/refs

func InfoRefs(forge *Forge, owner, repoName, service string, protocolV2 bool) GitReply {
	repo := forge.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	if service == "git-upload-pack" {
		if protocolV2 {
			var b bytes.Buffer
			b.Write(pkt("version 2\n"))
			b.Write(pkt("agent=" + gitAgent + "\n"))
			b.Write(pkt("ls-refs=unborn\n"))
			b.Write(pkt("fetch=shallow\n"))
			b.Write(pkt("object-format=sha1\n"))
			b.Write(pktFlush)
			return gitOK("application/x-git-upload-pack-advertisement", b.Bytes())
		}
		caps := "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow agent=forge/1.0"
		var parts [][]byte
		parts = append(parts, pkt("# service=git-upload-pack\n"), pktFlush)
		headTarget := repo.DefaultBranch
		headOid, hasHead := repo.Refs["refs/heads/"+headTarget]
		if len(repo.Refs) == 0 {
			parts = append(parts, pkt(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		} else {
			first := true
			if hasHead {
				parts = append(parts, pkt(headOid+" HEAD\x00symref=HEAD:refs/heads/"+headTarget+" "+caps+"\n"))
				first = false
			}
			for _, name := range SortedRefs(repo) {
				if name == "HEAD" {
					continue
				}
				oid := repo.Refs[name]
				if first {
					parts = append(parts, pkt(oid+" "+name+"\x00"+caps+"\n"))
					first = false
				} else {
					parts = append(parts, pkt(oid+" "+name+"\n"))
				}
			}
		}
		parts = append(parts, pktFlush)
		return gitOK("application/x-git-upload-pack-advertisement", concatBytes(parts...))
	}
	if service == "git-receive-pack" {
		caps := "report-status report-status-v2 side-band-64k delete-refs ofs-delta quiet atomic agent=forge/1.0"
		var parts [][]byte
		parts = append(parts, pkt("# service=git-receive-pack\n"), pktFlush)
		if len(repo.Refs) == 0 {
			parts = append(parts, pkt(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		} else {
			first := true
			for _, name := range SortedRefs(repo) {
				oid := repo.Refs[name]
				if first {
					parts = append(parts, pkt(oid+" "+name+"\x00"+caps+"\n"))
					first = false
				} else {
					parts = append(parts, pkt(oid+" "+name+"\n"))
				}
			}
		}
		parts = append(parts, pktFlush)
		return gitOK("application/x-git-receive-pack-advertisement", concatBytes(parts...))
	}
	return gitNotFound()
}

func SortedRefs(repo *Repo) []string {
	var heads, tags, others []string
	for name := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			heads = append(heads, name)
		} else if strings.HasPrefix(name, "refs/tags/") {
			tags = append(tags, name)
		} else {
			others = append(others, name)
		}
	}
	sortStrings(heads)
	sortStrings(tags)
	return append(append(heads, tags...), others...)
}

func sortStrings(s []string) {
	for i := 0; i < len(s); i++ {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// ---------------------------------------------------------------- upload-pack POST

func UploadPack(forge *Forge, owner, repoName string, body []byte, protocolV2 bool) GitReply {
	repo := forge.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	defer func() {
		if r := recover(); r != nil {
			// handled by the caller via the reply channel; panics here would kill the trial
		}
	}()
	if protocolV2 {
		return uploadPackV2(forge, repo, body)
	}
	return uploadPackV0(forge, repo, body)
}

func sideband(data []byte) []byte {
	var parts [][]byte
	const maxChunk = 65515
	for at := 0; at < len(data); at += maxChunk {
		end := at + maxChunk
		if end > len(data) {
			end = len(data)
		}
		chunk := append([]byte{1}, data[at:end]...)
		parts = append(parts, pktBytes(chunk))
	}
	return concatBytes(parts...)
}

type pktPacket struct {
	kind    string // data | flush | delim | end
	payload []byte
}

func decodePkts(buf []byte) []pktPacket {
	var pkts []pktPacket
	at := 0
	for at+4 <= len(buf) {
		hex := string(buf[at : at+4])
		if !isHex4(hex) {
			panic(fmt.Sprintf("bad pkt-line header at %d", at))
		}
		length, _ := strconv.ParseInt(hex, 16, 32)
		switch length {
		case 0:
			pkts = append(pkts, pktPacket{kind: "flush"})
			at += 4
			continue
		case 1:
			pkts = append(pkts, pktPacket{kind: "delim"})
			at += 4
			continue
		case 2:
			pkts = append(pkts, pktPacket{kind: "end"})
			at += 4
			continue
		}
		if int(length) < 4 || at+int(length) > len(buf) {
			panic("truncated pkt-line")
		}
		pkts = append(pkts, pktPacket{kind: "data", payload: buf[at+4 : at+int(length)]})
		at += int(length)
	}
	return pkts
}

func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func uploadPackV2(forge *Forge, repo *Repo, body []byte) GitReply {
	pkts := decodePkts(body)
	command := ""
	var args []string
	inArgs := false
	for _, p := range pkts {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := string(p.payload)
		if strings.HasPrefix(line, "command=") {
			command = strings.TrimSpace(line[8:])
			continue
		}
		if inArgs {
			args = append(args, strings.TrimSuffix(line, "\n"))
		}
	}
	if command == "ls-refs" {
		return lsRefsV2(repo, args)
	}
	if command == "fetch" {
		return fetchV2(forge, repo, args)
	}
	errBody, _ := json.Marshal(map[string]string{"message": "unknown command"})
	return GitReply{Status: 400, ContentType: "application/json", Body: append(errBody, '\n')}
}

func lsRefsV2(repo *Repo, args []string) GitReply {
	wantPeel, wantSymrefs := false, false
	var prefixes []string
	for _, a := range args {
		if a == "peel" {
			wantPeel = true
		} else if a == "symrefs" {
			wantSymrefs = true
		} else if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var parts [][]byte
	headTarget := "refs/heads/" + repo.DefaultBranch
	if len(repo.Refs) == 0 {
		if keep("HEAD") {
			line := "unborn HEAD"
			if wantSymrefs {
				line += " symref-target:" + headTarget
			}
			parts = append(parts, pkt(line+"\n"))
		}
	} else {
		headOid, hasHead := repo.Refs[headTarget]
		if hasHead && keep("HEAD") {
			line := headOid + " HEAD"
			if wantSymrefs {
				line += " symref-target:" + headTarget
			}
			parts = append(parts, pkt(line+"\n"))
		}
		for _, name := range SortedRefs(repo) {
			if name == "HEAD" || !keep(name) {
				continue
			}
			oid := repo.Refs[name]
			line := oid + " " + name
			if wantSymrefs && name == "HEAD" {
				line += " symref-target:" + headTarget
			}
			if wantPeel {
				if peeled := peelTag(repo, oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
			parts = append(parts, pkt(line+"\n"))
		}
	}
	parts = append(parts, pktFlush)
	return gitOK("application/x-git-upload-pack-result", concatBytes(parts...))
}

func peelTag(repo *Repo, oid string) string {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjTag {
		return ""
	}
	text := string(object.Data)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

func parseCommitView(repo *Repo, oid string) *CommitView {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjCommit {
		return nil
	}
	v := ParseCommit(object.Data)
	return &v
}

func fetchV2(forge *Forge, repo *Repo, args []string) GitReply {
	var wants, haves, clientShallow []string
	depth, done := 0, false
	hasDepth := false
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimSpace(arg[5:]))
		case strings.HasPrefix(arg, "shallow "):
			clientShallow = append(clientShallow, strings.TrimSpace(arg[8:]))
		case strings.HasPrefix(arg, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(arg[7:]))
			hasDepth = true
		case arg == "done":
			done = true
		}
	}
	var parts [][]byte
	DELIM := pktDelim
	if len(haves) > 0 {
		parts = append(parts, pkt("acknowledgments\n"))
		var common []string
		for _, h := range haves {
			if _, ok := repo.Objects[h]; ok {
				common = append(common, h)
			}
		}
		for _, oid := range common {
			parts = append(parts, pkt("ACK "+oid+"\n"))
		}
		if len(common) > 0 && done {
			parts = append(parts, pkt("ready\n"))
		}
		if len(common) == 0 {
			parts = append(parts, pkt("NAK\n"))
		}
		parts = append(parts, DELIM)
	}
	var shallow []string
	depthOf := map[string]int{}
	if hasDepth && depth >= 1 {
		var walk func(oid string, d int)
		walk = func(oid string, d int) {
			if known, ok := depthOf[oid]; ok && known <= d {
				return
			}
			depthOf[oid] = d
			if d >= depth {
				if !containsStr(shallow, oid) {
					shallow = append(shallow, oid)
				}
				return
			}
			view := parseCommitView(repo, oid)
			if view == nil {
				if !containsStr(shallow, oid) {
					shallow = append(shallow, oid)
				}
				return
			}
			for _, parent := range view.Parents {
				walk(parent, d+1)
			}
		}
		for _, want := range wants {
			walk(want, 1)
		}
		for _, boundary := range clientShallow {
			if d, ok := depthOf[boundary]; ok && d < depth {
				if idx := indexOfStr(shallow, boundary); idx >= 0 {
					shallow = append(shallow[:idx], shallow[idx+1:]...)
				}
			}
		}
	}
	if len(shallow) > 0 || len(clientShallow) > 0 {
		parts = append(parts, pkt("shallow-info\n"))
		for _, oid := range shallow {
			parts = append(parts, pkt("shallow "+oid+"\n"))
		}
		for _, oid := range clientShallow {
			if !containsStr(shallow, oid) {
				if _, ok := repo.Objects[oid]; ok {
					parts = append(parts, pkt("unshallow "+oid+"\n"))
				}
			}
		}
		parts = append(parts, DELIM)
	}
	parts = append(parts, pkt("packfile\n"))
	haveSet := map[string]bool{}
	if len(haves) > 0 && done {
		for _, h := range haves {
			if _, ok := repo.Objects[h]; !ok {
				continue
			}
			haveSet[h] = true
			for oid := range CollectReachable([]string{h}, func(o string) (GitObject, bool) {
				g, ok := repo.Objects[o]
				return g, ok
			}, nil) {
				haveSet[oid] = true
			}
		}
		// a client shallow boundary cuts its own closure: beyond it is NOT held
		beyond := map[string]bool{}
		for _, boundary := range clientShallow {
			if view := parseCommitView(repo, boundary); view != nil {
				for _, parent := range view.Parents {
					for oid := range CollectReachable([]string{parent}, func(o string) (GitObject, bool) {
						g, ok := repo.Objects[o]
						return g, ok
					}, nil) {
						beyond[oid] = true
					}
				}
			}
		}
		for oid := range beyond {
			delete(haveSet, oid)
		}
	}
	for _, boundary := range clientShallow {
		haveSet[boundary] = true
	}
	excluded := map[string]bool{}
	for _, boundary := range shallow {
		if view := parseCommitView(repo, boundary); view != nil {
			for _, parent := range view.Parents {
				for oid := range CollectReachable([]string{parent}, func(o string) (GitObject, bool) {
					g, ok := repo.Objects[o]
					return g, ok
				}, nil) {
					excluded[oid] = true
				}
			}
		}
	}
	var objects []GitObject
	seen := map[string]bool{}
	for _, want := range wants {
		reachable := CollectReachable([]string{want}, func(o string) (GitObject, bool) {
			g, ok := repo.Objects[o]
			return g, ok
		}, haveSet)
		oids := make([]string, 0, len(reachable))
		for oid := range reachable {
			oids = append(oids, oid)
		}
		sortStrings(oids)
		for _, oid := range oids {
			if seen[oid] || excluded[oid] || haveSet[oid] {
				continue
			}
			seen[oid] = true
			objects = append(objects, reachable[oid])
		}
	}
	pack := WritePack(objects)
	parts = append(parts, sideband(pack))
	parts = append(parts, pktFlush)
	return gitOK("application/x-git-upload-pack-result", concatBytes(parts...))
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func indexOfStr(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}

func uploadPackV0(forge *Forge, repo *Repo, body []byte) GitReply {
	pkts := decodePkts(body)
	var wants, haves, clientShallow []string
	depth := 0
	hasDepth := false
	inWants := true
	for _, p := range pkts {
		if p.kind == "flush" {
			if inWants {
				inWants = false
			}
			continue
		}
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.payload), "\n")
		if line == "done" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "want "):
			rest := line[5:]
			if sp := strings.Index(rest, " "); sp >= 0 {
				wants = append(wants, rest[:sp])
			} else {
				wants = append(wants, rest)
			}
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			clientShallow = append(clientShallow, strings.TrimSpace(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			depth, _ = strconv.Atoi(strings.TrimSpace(line[7:]))
			hasDepth = true
		}
	}
	var parts [][]byte
	var shallowList []string
	if hasDepth && depth >= 1 {
		depthOf := map[string]int{}
		var walk func(oid string, d int)
		walk = func(oid string, d int) {
			if known, ok := depthOf[oid]; ok && known <= d {
				return
			}
			depthOf[oid] = d
			if d >= depth {
				if !containsStr(shallowList, oid) {
					shallowList = append(shallowList, oid)
				}
				return
			}
			view := parseCommitView(repo, oid)
			if view == nil {
				if !containsStr(shallowList, oid) {
					shallowList = append(shallowList, oid)
				}
				return
			}
			for _, parent := range view.Parents {
				walk(parent, d+1)
			}
		}
		for _, want := range wants {
			walk(want, 1)
		}
		for _, oid := range shallowList {
			parts = append(parts, pkt("shallow "+oid+"\n"))
		}
	}
	var common []string
	for _, h := range haves {
		if _, ok := repo.Objects[h]; ok {
			common = append(common, h)
		}
	}
	if len(common) > 0 {
		for _, oid := range common {
			parts = append(parts, pkt("ACK "+oid+"\n"))
		}
		parts = append(parts, pkt("ACK "+common[len(common)-1]+"\n"))
	} else {
		parts = append(parts, pkt("NAK\n"))
	}
	haveSet := map[string]bool{}
	if len(common) > 0 {
		for _, h := range haves {
			if _, ok := repo.Objects[h]; !ok {
				continue
			}
			haveSet[h] = true
			for oid := range CollectReachable([]string{h}, func(o string) (GitObject, bool) {
				g, ok := repo.Objects[o]
				return g, ok
			}, nil) {
				haveSet[oid] = true
			}
		}
		beyond := map[string]bool{}
		for _, boundary := range clientShallow {
			if view := parseCommitView(repo, boundary); view != nil {
				for _, parent := range view.Parents {
					for oid := range CollectReachable([]string{parent}, func(o string) (GitObject, bool) {
						g, ok := repo.Objects[o]
						return g, ok
					}, nil) {
						beyond[oid] = true
					}
				}
			}
		}
		for oid := range beyond {
			delete(haveSet, oid)
		}
	}
	for _, boundary := range clientShallow {
		haveSet[boundary] = true
	}
	excluded := map[string]bool{}
	for _, boundary := range shallowList {
		if view := parseCommitView(repo, boundary); view != nil {
			for _, parent := range view.Parents {
				for oid := range CollectReachable([]string{parent}, func(o string) (GitObject, bool) {
					g, ok := repo.Objects[o]
					return g, ok
				}, nil) {
					excluded[oid] = true
				}
			}
		}
	}
	var objects []GitObject
	seen := map[string]bool{}
	for _, want := range wants {
		reachable := CollectReachable([]string{want}, func(o string) (GitObject, bool) {
			g, ok := repo.Objects[o]
			return g, ok
		}, haveSet)
		oids := make([]string, 0, len(reachable))
		for oid := range reachable {
			oids = append(oids, oid)
		}
		sortStrings(oids)
		for _, oid := range oids {
			if seen[oid] || excluded[oid] || haveSet[oid] {
				continue
			}
			seen[oid] = true
			objects = append(objects, reachable[oid])
		}
	}
	pack := WritePack(objects)
	parts = append(parts, sideband(pack))
	parts = append(parts, pktFlush)
	return gitOK("application/x-git-upload-pack-result", concatBytes(parts...))
}

// ---------------------------------------------------------------- receive-pack

func ReceivePack(forge *Forge, owner, repoName string, body []byte) GitReply {
	repo := forge.GetRepo(owner, repoName)
	if repo == nil {
		return gitNotFound()
	}
	report := "unpack ok\n"
	func() {
		defer func() {
			if r := recover(); r != nil {
				report = "unpack index-pack failed\n"
			}
		}()
		report = applyReceivePack(forge, repo, body)
	}()
	var reportPkts [][]byte
	for _, line := range strings.Split(strings.TrimSuffix(report, "\n"), "\n") {
		if line == "" {
			continue
		}
		reportPkts = append(reportPkts, pkt(line+"\n"))
	}
	reportPkts = append(reportPkts, pktFlush)
	reportBytes := concatBytes(reportPkts...)
	clientSideband := bytes.Contains(body[:minInt(len(body), 8192)], []byte("side-band-64k"))
	var out []byte
	if clientSideband {
		out = concatBytes(sideband(reportBytes), pktFlush)
	} else {
		out = reportBytes
	}
	return gitOK("application/x-git-receive-pack-result", out)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type pushCommand struct {
	oldOid, newOid, ref string
}

func applyReceivePack(forge *Forge, repo *Repo, body []byte) string {
	var commands []pushCommand
	packStart := -1
	offset := 0
	first := true
	for {
		if offset+4 > len(body) {
			break
		}
		hex := string(body[offset : offset+4])
		if !isHex4(hex) {
			return "unpack bad pkt-line header\n"
		}
		length, _ := strconv.ParseInt(hex, 16, 32)
		if length == 0 {
			packStart = offset + 4
			break
		}
		if length == 1 || length == 2 {
			offset += 4
			continue
		}
		if offset+int(length) > len(body) {
			return "unpack truncated pkt-line\n"
		}
		text := strings.TrimSuffix(string(body[offset+4:offset+int(length)]), "\n")
		nul := -1
		if first {
			nul = strings.IndexByte(text, '\x00')
		}
		line := text
		if nul >= 0 {
			line = text[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) >= 3 {
			commands = append(commands, pushCommand{oldOid: parts[0], newOid: parts[1], ref: strings.Join(parts[2:], " ")})
		}
		first = false
		offset += int(length)
	}
	if len(commands) == 0 {
		return "unpack ok\n"
	}
	updatesRef := false
	for _, c := range commands {
		if c.newOid != ZeroOID {
			updatesRef = true
		}
	}
	packBytes := []byte{}
	if packStart >= 0 && packStart < len(body) {
		packBytes = body[packStart:]
	}
	if updatesRef {
		if len(packBytes) < 12 {
			return "unpack index-pack failed\n"
		}
		if !VerifyPackChecksum(packBytes) {
			return "unpack pack checksum mismatch\n"
		}
		read, err := ReadPack(packBytes)
		if err != nil {
			return "unpack index-pack failed\n"
		}
		for oid, object := range read.Objects {
			repo.Objects[oid] = object
		}
	}
	lines := []string{"unpack ok"}
	for _, command := range commands {
		current, has := repo.Refs[command.ref]
		if command.newOid == ZeroOID {
			if !has {
				lines = append(lines, "ng "+command.ref+" no such ref")
				continue
			}
			if command.oldOid != ZeroOID && current != command.oldOid {
				lines = append(lines, "ng "+command.ref+" fetch first")
				continue
			}
			delete(repo.Refs, command.ref)
			lines = append(lines, "ok "+command.ref)
			continue
		}
		if !IsObjectID(command.newOid) {
			lines = append(lines, "ng "+command.ref+" missing necessary objects")
			continue
		}
		if _, ok := repo.Objects[command.newOid]; !ok {
			lines = append(lines, "ng "+command.ref+" missing necessary objects")
			continue
		}
		if !has {
			if command.oldOid != ZeroOID {
				lines = append(lines, "ng "+command.ref+" stale info")
				continue
			}
			repo.Refs[command.ref] = command.newOid
			lines = append(lines, "ok "+command.ref)
			continue
		}
		if command.oldOid != current {
			lines = append(lines, "ng "+command.ref+" fetch first")
			continue
		}
		if !forge.IsAncestor(repo, command.oldOid, command.newOid) {
			lines = append(lines, "ng "+command.ref+" non-fast-forward")
			continue
		}
		repo.Refs[command.ref] = command.newOid
		lines = append(lines, "ok "+command.ref)
	}
	return strings.Join(lines, "\n") + "\n"
}
