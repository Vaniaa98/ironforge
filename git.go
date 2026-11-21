package main

// Git smart HTTP: upload-pack protocol v2 (ls-refs, fetch, shallow) and
// receive-pack v0 (report-status, side-band-64k, delete-refs, fast-forward
// only), plus the object-model helpers the REST worker builds on.
//
// Protocol source of truth: program/env/src/git-client.ts (the suite's own
// client). Notable wire facts taken from it:
//   - pack entries carry the object's RAW bytes (no loose header) under zlib;
//   - the v2 advertisement opens with "# service=..." then "version 2";
//   - fetch v2 answers sections acknowledgments / shallow-info / packfile
//     with the pack inside sideband band 1;
//   - the push report is a pkt-line stream ("unpack ok", "ok <ref>",
//     "ng <ref> <reason>") wrapped in sideband band 1;
//   - the client always sends Authorization: Bearer <token> on git calls.

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"

// gitIdentity is the fixed author/committer line every server-created
// commit carries (fixture epoch, no wall clock).
const gitIdentity = "ada iversen <adaiversen@example.invalid> 1735689600 +0000"

const gitAgent = "forge/1.0"

// ---------------------------------------------------------------------------
// object model
// ---------------------------------------------------------------------------

// treeEntryRaw is one parsed entry of a tree object.
type treeEntryRaw struct {
	Mode string
	Name string
	SHA  string // hex
}

// parseTreeEntries parses a tree object's raw bytes. Corrupt data yields nil.
func parseTreeEntries(data []byte) []treeEntryRaw {
	var out []treeEntryRaw
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil
		}
		mode := string(data[off : off+sp])
		rest := off + sp + 1
		nul := bytes.IndexByte(data[rest:], 0)
		if nul < 0 || rest+nul+21 > len(data) {
			return nil
		}
		name := string(data[rest : rest+nul])
		sha := hex.EncodeToString(data[rest+nul+1 : rest+nul+21])
		out = append(out, treeEntryRaw{Mode: mode, Name: name, SHA: sha})
		off = rest + nul + 21
	}
	return out
}

// parseCommitHeaders extracts the tree and parent lines of a commit object.
func parseCommitHeaders(data []byte) (tree string, parents []string) {
	head := data
	if end := bytes.Index(data, []byte("\n\n")); end >= 0 {
		head = data[:end]
	}
	for _, line := range strings.Split(string(head), "\n") {
		if strings.HasPrefix(line, "tree ") {
			tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			parents = append(parents, line[7:])
		}
	}
	return tree, parents
}

// parseTagTarget returns the object a tag points at ("" if not a tag).
func parseTagTarget(data []byte) string {
	head := data
	if end := bytes.Index(data, []byte("\n\n")); end >= 0 {
		head = data[:end]
	}
	for _, line := range strings.Split(string(head), "\n") {
		if strings.HasPrefix(line, "object ") {
			return line[7:]
		}
	}
	return ""
}

func isDirMode(mode string) bool { return mode == "40000" || mode == "040000" }

// treeSortLess implements git's tree sort: names compared as bytes,
// directories as if the name ended in '/'.
func treeSortLess(modeA, nameA, modeB, nameB string) bool {
	a := nameA
	if isDirMode(modeA) {
		a += "/"
	}
	b := nameB
	if isDirMode(modeB) {
		b += "/"
	}
	return a < b
}

func encodeTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return treeSortLess(sorted[i].Mode, sorted[i].Name, sorted[j].Mode, sorted[j].Name)
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		raw, err := hex.DecodeString(e.SHA)
		if err != nil || len(raw) != 20 {
			continue
		}
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// peelToCommit follows tag objects until a commit ("" when the chain leaves
// the object graph or is not a commit at all).
func (g *GitRepo) peelToCommit(oid string) string {
	for i := 0; i < 16; i++ {
		obj := g.Objects[oid]
		if obj == nil {
			return ""
		}
		if obj.Type == "commit" {
			return oid
		}
		if obj.Type != "tag" {
			return ""
		}
		oid = parseTagTarget(obj.Data)
		if oid == "" {
			return ""
		}
	}
	return ""
}

// peelTag follows tags until a non-tag object (usually the tagged commit).
func (g *GitRepo) peelTag(oid string) string {
	cur := oid
	for i := 0; i < 16; i++ {
		obj := g.Objects[cur]
		if obj == nil {
			return ""
		}
		if obj.Type != "tag" {
			return cur
		}
		cur = parseTagTarget(obj.Data)
		if cur == "" {
			return ""
		}
	}
	return ""
}

// collectReachable returns every object reachable from roots. Missing
// objects are skipped.
func (g *GitRepo) collectReachable(roots []string) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || seen[oid] {
			continue
		}
		obj := g.Objects[oid]
		if obj == nil {
			continue
		}
		seen[oid] = true
		switch obj.Type {
		case "commit":
			tree, parents := parseCommitHeaders(obj.Data)
			if tree != "" {
				stack = append(stack, tree)
			}
			stack = append(stack, parents...)
		case "tag":
			if t := parseTagTarget(obj.Data); t != "" {
				stack = append(stack, t)
			}
		case "tree":
			for _, e := range parseTreeEntries(obj.Data) {
				if e.Mode != "160000" {
					stack = append(stack, e.SHA)
				}
			}
		}
	}
	return seen
}

func sortedRefNames(g *GitRepo) []string {
	names := make([]string, 0, len(g.Refs))
	for name := range g.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// helpers used by the REST worker (exact names/signatures; callers hold the
// store lock — these methods do not lock themselves)
// ---------------------------------------------------------------------------

// WriteBlob stores a blob and returns its SHA-1 hex.
func (g *GitRepo) WriteBlob(data []byte) string {
	return g.WriteObject("blob", data)
}

// TreeEntry is one entry of a tree to write.
type TreeEntry struct {
	Mode, Name, SHA string
}

// WriteTree stores a tree built from entries (git-sorted) and returns its SHA-1 hex.
func (g *GitRepo) WriteTree(entries []TreeEntry) string {
	return g.WriteObject("tree", encodeTree(entries))
}

// WriteCommit stores a commit with the fixed fixture identity and returns
// its SHA-1 hex. The message gets a trailing newline when it lacks one.
func (g *GitRepo) WriteCommit(treeSHA string, parents []string, message string) string {
	var b strings.Builder
	b.WriteString("tree " + treeSHA + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + gitIdentity + "\n")
	b.WriteString("committer " + gitIdentity + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return g.WriteObject("commit", []byte(b.String()))
}

// UpdateRef moves a ref fast-forward only. newSHA "" deletes the ref.
// Returns ok=false with reason "non-fast-forward" when the update is not a
// descendant of the current value.
func (g *GitRepo) UpdateRef(ref, newSHA string) (bool, string) {
	if newSHA == "" {
		if _, ok := g.Refs[ref]; !ok {
			return false, "remote ref does not exist"
		}
		delete(g.Refs, ref)
		g.PackValid = false
		g.Rev++
		g.FetchCache = nil
		return true, ""
	}
	if _, ok := g.Objects[newSHA]; !ok {
		return false, "object not found"
	}
	cur, exists := g.Refs[ref]
	if !exists {
		g.Refs[ref] = newSHA
		g.PackValid = false
		g.Rev++
		g.FetchCache = nil
		return true, ""
	}
	if cur == newSHA {
		return true, ""
	}
	if !g.IsAncestor(cur, newSHA) {
		return false, "non-fast-forward"
	}
	g.Refs[ref] = newSHA
	g.PackValid = false
	g.Rev++
	g.FetchCache = nil
	return true, ""
}

// IsAncestor reports whether ancestor is reachable from descendant by
// walking commit parents (tag objects are peeled first).
func (g *GitRepo) IsAncestor(ancestor, descendant string) bool {
	a := g.peelToCommit(ancestor)
	d := g.peelToCommit(descendant)
	if a == "" || d == "" {
		return ancestor == descendant
	}
	if a == d {
		return true
	}
	seen := map[string]bool{d: true}
	queue := []string{d}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		obj := g.Objects[cur]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents := parseCommitHeaders(obj.Data)
		for _, p := range parents {
			if p == a {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false
}

// CommitTreeSHA returns the tree SHA of a commit ("" when unknown/not a commit).
func (g *GitRepo) CommitTreeSHA(commitSHA string) string {
	obj := g.Objects[commitSHA]
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	tree, _ := parseCommitHeaders(obj.Data)
	return tree
}

// HeadTreeSHA returns the tree SHA of the commit a ref points at.
func (g *GitRepo) HeadTreeSHA(ref string) string {
	return g.CommitTreeSHA(g.Refs[ref])
}

// treeAtCommit walks path segments ("" = root) and returns the tree SHA.
func (g *GitRepo) treeAtCommit(commitSHA, dir string) string {
	treeSHA := g.CommitTreeSHA(commitSHA)
	if treeSHA == "" {
		return ""
	}
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return treeSHA
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" {
			continue
		}
		obj := g.Objects[treeSHA]
		if obj == nil || obj.Type != "tree" {
			return ""
		}
		next := ""
		for _, e := range parseTreeEntries(obj.Data) {
			if e.Name == seg && isDirMode(e.Mode) {
				next = e.SHA
				break
			}
		}
		if next == "" {
			return ""
		}
		treeSHA = next
	}
	return treeSHA
}

// GetFileAtCommit returns a file's bytes at a commit, walking trees.
func (g *GitRepo) GetFileAtCommit(commitSHA, path string) ([]byte, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return nil, false
	}
	treeSHA := g.CommitTreeSHA(commitSHA)
	if treeSHA == "" {
		return nil, false
	}
	for i, seg := range parts {
		obj := g.Objects[treeSHA]
		if obj == nil || obj.Type != "tree" {
			return nil, false
		}
		var found *treeEntryRaw
		for _, e := range parseTreeEntries(obj.Data) {
			if e.Name == seg {
				ee := e
				found = &ee
				break
			}
		}
		if found == nil {
			return nil, false
		}
		if i == len(parts)-1 {
			if isDirMode(found.Mode) {
				return nil, false
			}
			blob := g.Objects[found.SHA]
			if blob == nil || blob.Type != "blob" {
				return nil, false
			}
			return blob.Data, true
		}
		treeSHA = found.SHA
	}
	return nil, false
}

// DirEntry is one entry of ListDirAtCommit.
type DirEntry struct {
	Name, Path, Type string
	SHA              string
}

// ListDirAtCommit lists a directory's entries at a commit (dir "" = root).
// Type is "file" or "dir".
func (g *GitRepo) ListDirAtCommit(commitSHA, dir string) []DirEntry {
	treeSHA := g.treeAtCommit(commitSHA, dir)
	if treeSHA == "" {
		return nil
	}
	obj := g.Objects[treeSHA]
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	var out []DirEntry
	for _, e := range parseTreeEntries(obj.Data) {
		typ := "file"
		if isDirMode(e.Mode) {
			typ = "dir"
		}
		p := e.Name
		if d := strings.Trim(dir, "/"); d != "" {
			p = d + "/" + e.Name
		}
		out = append(out, DirEntry{Name: e.Name, Path: p, Type: typ, SHA: e.SHA})
	}
	return out
}

// ---------------------------------------------------------------------------
// pkt-line framing
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	const hexd = "0123456789abcdef"
	n := len(payload) + 4
	out := make([]byte, 4, len(payload)+4)
	out[0] = hexd[(n>>12)&0xf]
	out[1] = hexd[(n>>8)&0xf]
	out[2] = hexd[(n>>4)&0xf]
	out[3] = hexd[n&0xf]
	return append(out, payload...)
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func isHex4(b []byte) bool {
	if len(b) != 4 {
		return false
	}
	for _, c := range b {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// parsePktStream decodes pkt-lines until a flush packet or until the bytes
// stop looking like pkt-lines (raw pack data follows). It returns the data
// payloads, the remaining bytes, and whether a flush was seen. Delim and
// response-end packets are skipped.
func parsePktStream(data []byte) (payloads [][]byte, rest []byte, flushed bool) {
	for len(data) >= 4 {
		if !isHex4(data[:4]) {
			return payloads, data, false
		}
		n, err := strconv.ParseInt(string(data[:4]), 16, 32)
		if err != nil || n < 0 || n > 65520 {
			return payloads, data, false
		}
		if n == 0 {
			return payloads, data[4:], true
		}
		if n == 1 || n == 2 {
			data = data[4:]
			continue
		}
		if n == 3 || int(n) > len(data) {
			return payloads, data, false
		}
		payloads = append(payloads, data[4:n])
		data = data[n:]
	}
	return payloads, data, false
}

// parseAllPktLines decodes every pkt-line in data until the bytes stop
// looking like pkt-lines (raw pack data). Flush/delim packets are dropped.
func parseAllPktLines(data []byte) (payloads [][]byte, rest []byte) {
	for len(data) >= 4 {
		if !isHex4(data[:4]) {
			return payloads, data
		}
		n, err := strconv.ParseInt(string(data[:4]), 16, 32)
		if err != nil || n < 0 || n > 65520 {
			return payloads, data
		}
		if n == 0 || n == 1 || n == 2 {
			data = data[4:]
			continue
		}
		if n == 3 || int(n) > len(data) {
			return payloads, data
		}
		payloads = append(payloads, data[4:n])
		data = data[n:]
	}
	return payloads, data
}

// sidebandWrap wraps raw bytes into side-band-64k pkt-lines on band 1.
func sidebandWrap(data []byte) []byte {
	const maxChunk = 65515 // 65520 - 4 (pkt header) - 1 (band byte)
	var out bytes.Buffer
	for len(data) > 0 {
		n := len(data)
		if n > maxChunk {
			n = maxChunk
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, data[:n]...)
		out.Write(pktLine(chunk))
		data = data[n:]
	}
	return out.Bytes()
}

// writeGitResponse writes a complete git response with an explicit length.
func writeGitResponse(w http.ResponseWriter, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// advertisements
// ---------------------------------------------------------------------------

// v2Advertisement builds the upload-pack protocol v2 capability advertisement.
func v2Advertisement() []byte {
	var out bytes.Buffer
	out.Write(pktLineStr("# service=git-upload-pack\n"))
	out.Write(pktFlush)
	out.Write(pktLineStr("version 2\n"))
	out.Write(pktLineStr("ls-refs=unborn\n"))
	out.Write(pktLineStr("fetch=shallow wait-for-done\n"))
	out.Write(pktLineStr("object-format=sha1\n"))
	out.Write(pktLineStr("agent=" + gitAgent + "\n"))
	out.Write(pktFlush)
	return out.Bytes()
}

// v0Advertisement builds a v0 ref advertisement for service. caps are
// advertised on the first line after a NUL byte; peeled tag lines follow
// their tag line. An empty repo advertises the capabilities^{} pseudo-ref.
func v0Advertisement(g *GitRepo, service, caps string) []byte {
	var out bytes.Buffer
	out.Write(pktLineStr("# service=" + service + "\n"))
	out.Write(pktFlush)
	mainTip := g.Refs["refs/heads/"+DefaultBranch]
	var lines []string
	if mainTip == "" && len(g.Refs) == 0 {
		lines = []string{zeroOID + " capabilities^{}"}
	} else {
		if mainTip != "" {
			lines = append(lines, mainTip+" HEAD")
		} else {
			lines = append(lines, zeroOID+" HEAD")
		}
		for _, name := range sortedRefNames(g) {
			oid := g.Refs[name]
			lines = append(lines, oid+" "+name)
			if obj := g.Objects[oid]; obj != nil && obj.Type == "tag" {
				if peeled := g.peelTag(oid); peeled != "" && peeled != oid {
					lines = append(lines, peeled+" "+name+"^{}")
				}
			}
		}
	}
	for i, line := range lines {
		if i == 0 {
			line += "\x00" + caps
		}
		out.Write(pktLineStr(line + "\n"))
	}
	out.Write(pktFlush)
	return out.Bytes()
}

const v0UploadCaps = "side-band-64k thin-pack ofs-delta shallow multi_ack agent=" + gitAgent + " symref=HEAD:refs/heads/main"
const v0ReceiveCaps = "report-status side-band-64k delete-refs agent=" + gitAgent + " symref=HEAD:refs/heads/main"

// ---------------------------------------------------------------------------
// fetch planning
// ---------------------------------------------------------------------------

type fetchError string

func (e fetchError) Error() string { return string(e) }

// heldObjects computes the objects the client already holds: the closure of
// haves, not traversing past the client's shallow boundary commits (their
// parents are NOT held).
func (g *GitRepo) heldObjects(haves []string, shallow []string) map[string]bool {
	boundary := make(map[string]bool, len(shallow))
	for _, s := range shallow {
		boundary[s] = true
	}
	held := make(map[string]bool)
	stack := append([]string(nil), haves...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == "" || oid == zeroOID || held[oid] {
			continue
		}
		obj := g.Objects[oid]
		if obj == nil {
			continue
		}
		held[oid] = true
		switch obj.Type {
		case "commit":
			tree, parents := parseCommitHeaders(obj.Data)
			if tree != "" {
				stack = append(stack, tree)
			}
			if !boundary[oid] {
				stack = append(stack, parents...)
			}
		case "tag":
			if t := parseTagTarget(obj.Data); t != "" {
				stack = append(stack, t)
			}
		case "tree":
			for _, e := range parseTreeEntries(obj.Data) {
				if e.Mode != "160000" {
					stack = append(stack, e.SHA)
				}
			}
		}
	}
	return held
}

// fetchPlan is the resolved object set for one fetch.
type fetchPlan struct {
	objects   map[string]*GitObject
	shallow   []string
	unshallow []string
}

// planFetch resolves wants/haves/depth into the object set to send.
// depth counts generations from the wants (depth 1 = the tips only).
func (g *GitRepo) planFetch(wants, haves, clientShallow []string, depth int, hasDepth bool) (*fetchPlan, error) {
	held := g.heldObjects(haves, clientShallow)

	type item struct {
		oid   string
		depth int
	}
	dist := make(map[string]int)
	var queue []item
	var wantTags []string
	extraObjects := make(map[string]bool)
	enqueue := func(oid string, d int) {
		if _, ok := dist[oid]; ok {
			return
		}
		dist[oid] = d
		queue = append(queue, item{oid, d})
	}
	for _, w := range wants {
		obj := g.Objects[w]
		if obj == nil {
			return nil, fetchError("want " + w + " not found")
		}
		switch obj.Type {
		case "commit":
			enqueue(w, 0)
		case "tag":
			wantTags = append(wantTags, w)
			if target := g.peelToCommit(w); target != "" {
				enqueue(target, 0)
			} else if t := g.peelTag(w); t != "" {
				extraObjects[t] = true
			}
		default:
			extraObjects[w] = true
		}
	}
	sendCommits := make(map[string]bool)
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		sendCommits[it.oid] = true
		if hasDepth && it.depth+1 >= depth {
			continue
		}
		obj := g.Objects[it.oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents := parseCommitHeaders(obj.Data)
		for _, p := range parents {
			enqueue(p, it.depth+1)
		}
	}

	send := make(map[string]*GitObject)
	treeSeen := make(map[string]bool)
	var treeStack []string
	addTree := func(root string) {
		if root == "" {
			return
		}
		treeStack = append(treeStack, root)
		for len(treeStack) > 0 {
			t := treeStack[len(treeStack)-1]
			treeStack = treeStack[:len(treeStack)-1]
			if treeSeen[t] {
				continue
			}
			treeSeen[t] = true
			obj := g.Objects[t]
			if obj == nil || obj.Type != "tree" {
				continue
			}
			send[t] = obj
			for _, e := range parseTreeEntries(obj.Data) {
				if e.Mode == "160000" {
					continue
				}
				child := g.Objects[e.SHA]
				if child == nil {
					continue
				}
				if child.Type == "tree" {
					treeStack = append(treeStack, e.SHA)
				} else {
					send[e.SHA] = child
				}
			}
		}
	}
	for oid := range sendCommits {
		if held[oid] {
			continue
		}
		obj := g.Objects[oid]
		if obj == nil {
			continue
		}
		send[oid] = obj
		tree, _ := parseCommitHeaders(obj.Data)
		addTree(tree)
	}
	for _, w := range wantTags {
		cur := w
		for i := 0; i < 16; i++ {
			obj := g.Objects[cur]
			if obj == nil || obj.Type != "tag" {
				break
			}
			send[cur] = obj
			cur = parseTagTarget(obj.Data)
			if cur == "" {
				break
			}
		}
	}
	for oid := range extraObjects {
		if obj := g.Objects[oid]; obj != nil {
			send[oid] = obj
		}
	}
	// Never re-send what the client already holds.
	for oid := range held {
		delete(send, oid)
	}

	plan := &fetchPlan{objects: send}
	if !hasDepth && len(clientShallow) == 0 {
		return plan, nil
	}
	// Shallow boundary: sent commits with a parent outside the walked set.
	shallowSet := make(map[string]bool)
	for oid := range sendCommits {
		if held[oid] {
			continue
		}
		obj := g.Objects[oid]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents := parseCommitHeaders(obj.Data)
		for _, p := range parents {
			if !sendCommits[p] {
				shallowSet[oid] = true
				break
			}
		}
	}
	unshallowSet := make(map[string]bool)
	for _, b := range clientShallow {
		obj := g.Objects[b]
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents := parseCommitHeaders(obj.Data)
		allIn := len(parents) > 0
		for _, p := range parents {
			if !sendCommits[p] {
				allIn = false
				break
			}
		}
		if allIn {
			unshallowSet[b] = true
		} else {
			shallowSet[b] = true
		}
	}
	for oid := range shallowSet {
		plan.shallow = append(plan.shallow, oid)
	}
	for oid := range unshallowSet {
		plan.unshallow = append(plan.unshallow, oid)
	}
	sort.Strings(plan.shallow)
	sort.Strings(plan.unshallow)
	return plan, nil
}

// wantsCoverAllTips reports whether wants name every ref tip (a full clone).
func wantsCoverAllTips(g *GitRepo, wants []string) bool {
	set := make(map[string]bool, len(wants))
	for _, w := range wants {
		set[w] = true
	}
	for _, oid := range g.Refs {
		if !set[oid] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

// gitRepoName extracts owner/repo from the route; the route segment carries
// the ".git" suffix (ServeMux wildcards cannot have a literal suffix).
func gitRepoName(r *http.Request) (owner, repo string, ok bool) {
	owner = r.PathValue("owner")
	seg := r.PathValue("repo")
	if owner == "" || seg == "" || !strings.HasSuffix(seg, ".git") {
		return "", "", false
	}
	return owner, strings.TrimSuffix(seg, ".git"), true
}

func (s *Store) gitAuth(w http.ResponseWriter, r *http.Request) bool {
	if s.Auth(r) {
		return true
	}
	writeErr(w, s, Err(401, "Requires authentication"))
	return false
}

func gitInfoRefs(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.gitAuth(w, r) {
			return
		}
		owner, repo, ok := gitRepoName(r)
		if !ok {
			notFoundJSON(w, s)
			return
		}
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			notFoundJSON(w, s)
			return
		}
		rep := s.GetRepo(owner, repo)
		if rep == nil {
			writeErr(w, s, Err(404, "Not Found"))
			return
		}
		s.mu.RLock()
		defer s.mu.RUnlock()
		var body []byte
		var ctype string
		if service == "git-upload-pack" {
			ctype = "application/x-git-upload-pack-advertisement"
			if r.Header.Get("Git-Protocol") == "version=2" {
				body = v2Advertisement()
			} else {
				body = v0Advertisement(rep.Git, service, v0UploadCaps)
			}
		} else {
			ctype = "application/x-git-receive-pack-advertisement"
			body = v0Advertisement(rep.Git, service, v0ReceiveCaps)
		}
		writeGitResponse(w, ctype, body)
	}
}

// handleLsRefs answers a v2 ls-refs command.
func handleLsRefs(w http.ResponseWriter, s *Store, rep *Repo, payloads [][]byte) {
	var peel, symrefs, unborn bool
	var prefixes []string
	for _, p := range payloads {
		line := strings.TrimSuffix(string(p), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[len("ref-prefix "):])
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := rep.Git
	var out bytes.Buffer
	matches := func(name string) bool {
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
	mainTip := g.Refs["refs/heads/"+DefaultBranch]
	if matches("HEAD") {
		if mainTip != "" {
			line := mainTip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + DefaultBranch
			}
			out.Write(pktLineStr(line + "\n"))
		} else if unborn {
			line := "unborn HEAD"
			if symrefs {
				line += " symref-target:refs/heads/" + DefaultBranch
			}
			out.Write(pktLineStr(line + "\n"))
		}
	}
	for _, name := range sortedRefNames(g) {
		if !matches(name) {
			continue
		}
		oid := g.Refs[name]
		line := oid + " " + name
		if peel {
			if obj := g.Objects[oid]; obj != nil && obj.Type == "tag" {
				if peeled := g.peelTag(oid); peeled != "" && peeled != oid {
					line += " peeled:" + peeled
				}
			}
		}
		out.Write(pktLineStr(line + "\n"))
	}
	out.Write(pktFlush)
	writeGitResponse(w, "application/x-git-upload-pack-result", out.Bytes())
}

// fetchArgs is a parsed fetch request (v2 command=fetch, or v0 want/have).
type fetchArgs struct {
	wants, haves, shallow []string
	depth                 int
	hasDepth              bool
}

func parseFetchArgs(payloads [][]byte) fetchArgs {
	var a fetchArgs
	for _, p := range payloads {
		line := strings.TrimSuffix(string(p), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			if f := strings.Fields(line); len(f) >= 2 {
				a.wants = append(a.wants, f[1])
			}
		case strings.HasPrefix(line, "have "):
			if f := strings.Fields(line); len(f) >= 2 {
				a.haves = append(a.haves, f[1])
			}
		case strings.HasPrefix(line, "shallow "):
			if f := strings.Fields(line); len(f) >= 2 {
				a.shallow = append(a.shallow, f[1])
			}
		case strings.HasPrefix(line, "deepen "):
			if f := strings.Fields(line); len(f) >= 2 {
				if n, err := strconv.Atoi(f[1]); err == nil {
					a.depth = n
					a.hasDepth = true
				}
			}
		}
	}
	return a
}

// writeV2FetchPack writes a v2 fetch response: acknowledgments (NAK),
// optional shallow-info, and the packfile section (sideband band 1).
func buildV2FetchPack(pack []byte, shallow, unshallow []string) []byte {
	var out bytes.Buffer
	out.Write(pktLineStr("acknowledgments\n"))
	out.Write(pktLineStr("NAK\n"))
	out.Write(pktDelim)
	if len(shallow) > 0 || len(unshallow) > 0 {
		out.Write(pktLineStr("shallow-info\n"))
		for _, s := range shallow {
			out.Write(pktLineStr("shallow " + s + "\n"))
		}
		for _, s := range unshallow {
			out.Write(pktLineStr("unshallow " + s + "\n"))
		}
		out.Write(pktDelim)
	}
	out.Write(pktLineStr("packfile\n"))
	out.Write(sidebandWrap(pack))
	out.Write(pktFlush)
	return out.Bytes()
}

func writeV2FetchPack(w http.ResponseWriter, pack []byte, shallow, unshallow []string) {
	writeGitResponse(w, "application/x-git-upload-pack-result", buildV2FetchPack(pack, shallow, unshallow))
}

// writeV2FetchError answers a fetch whose want is unknown with a sideband
// band-3 error inside the packfile section.
func writeV2FetchError(w http.ResponseWriter, err error) {
	var out bytes.Buffer
	out.Write(pktLineStr("acknowledgments\n"))
	out.Write(pktLineStr("NAK\n"))
	out.Write(pktDelim)
	out.Write(pktLineStr("packfile\n"))
	msg := append([]byte{3}, []byte(err.Error()+"\n")...)
	out.Write(pktLine(msg))
	out.Write(pktFlush)
	writeGitResponse(w, "application/x-git-upload-pack-result", out.Bytes())
}

// fetchCacheKey identifies a v2 fetch request: the response is a pure
// function of the repo state at a given Rev plus these arguments.
func fetchCacheKey(args fetchArgs) string {
	hd := "0"
	if args.hasDepth {
		hd = "1"
	}
	return "w=" + strings.Join(args.wants, ",") + "|d=" + strconv.Itoa(args.depth) + "|hd=" + hd +
		"|h=" + strings.Join(args.haves, ",") + "|s=" + strings.Join(args.shallow, ",")
}

// fetchCacheStore records a response built at revision rev. A mutation
// between the read lock and this write lock bumps Rev (and clears the map),
// so a stale computation is never stored.
func (g *GitRepo) fetchCacheStore(s *Store, key string, resp []byte, rev uint64) {
	s.mu.Lock()
	if g.Rev == rev {
		if g.FetchCache == nil {
			g.FetchCache = make(map[string][]byte)
		}
		g.FetchCache[key] = resp
	}
	s.mu.Unlock()
}

// handleFetchV2 answers a v2 command=fetch. A full clone (no haves, no
// depth, every tip wanted) is served from the cached full pack, built once.
// Any fetch whose response was already built at the current revision is
// served from the fetch-response cache instead of re-planning + re-encoding.
func handleFetchV2(w http.ResponseWriter, s *Store, rep *Repo, payloads [][]byte) {
	args := parseFetchArgs(payloads)
	key := fetchCacheKey(args)
	s.mu.RLock()
	g := rep.Git
	if b, ok := g.FetchCache[key]; ok {
		s.mu.RUnlock()
		writeGitResponse(w, "application/x-git-upload-pack-result", b)
		return
	}
	rev := g.Rev
	fullClone := len(args.haves) == 0 && !args.hasDepth && len(args.shallow) == 0 && wantsCoverAllTips(g, args.wants)
	if fullClone {
		if !g.PackValid {
			s.mu.RUnlock()
			s.mu.Lock()
			if !g.PackValid {
				g.PackCache = buildFullPack(g)
				g.PackValid = true
			}
			pack := g.PackCache
			resp := buildV2FetchPack(pack, nil, nil)
			if g.Rev == rev {
				if g.FetchCache == nil {
					g.FetchCache = make(map[string][]byte)
				}
				g.FetchCache[key] = resp
			}
			s.mu.Unlock()
			writeGitResponse(w, "application/x-git-upload-pack-result", resp)
			return
		}
		pack := g.PackCache
		resp := buildV2FetchPack(pack, nil, nil)
		s.mu.RUnlock()
		g.fetchCacheStore(s, key, resp, rev)
		writeGitResponse(w, "application/x-git-upload-pack-result", resp)
		return
	}
	plan, err := g.planFetch(args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
	if err != nil {
		s.mu.RUnlock()
		writeV2FetchError(w, err)
		return
	}
	pack := encodePack(plan.objects)
	resp := buildV2FetchPack(pack, plan.shallow, plan.unshallow)
	s.mu.RUnlock()
	g.fetchCacheStore(s, key, resp, rev)
	writeGitResponse(w, "application/x-git-upload-pack-result", resp)
}

// handleFetchV0 answers a v0 fetch (want/have/done): NAK, shallow lines,
// then the sideband pack.
func handleFetchV0(w http.ResponseWriter, s *Store, rep *Repo, payloads [][]byte) {
	args := parseFetchArgs(payloads)
	s.mu.RLock()
	g := rep.Git
	plan, err := g.planFetch(args.wants, args.haves, args.shallow, args.depth, args.hasDepth)
	if err != nil {
		s.mu.RUnlock()
		var out bytes.Buffer
		out.Write(pktLineStr("NAK\n"))
		msg := append([]byte{3}, []byte(err.Error()+"\n")...)
		out.Write(pktLine(msg))
		out.Write(pktFlush)
		writeGitResponse(w, "application/x-git-upload-pack-result", out.Bytes())
		return
	}
	pack := encodePack(plan.objects)
	shallow, unshallow := plan.shallow, plan.unshallow
	s.mu.RUnlock()
	var out bytes.Buffer
	out.Write(pktLineStr("NAK\n"))
	for _, sh := range shallow {
		out.Write(pktLineStr("shallow " + sh + "\n"))
	}
	for _, sh := range unshallow {
		out.Write(pktLineStr("unshallow " + sh + "\n"))
	}
	out.Write(sidebandWrap(pack))
	out.Write(pktFlush)
	writeGitResponse(w, "application/x-git-upload-pack-result", out.Bytes())
}

func gitUploadPack(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.gitAuth(w, r) {
			return
		}
		owner, repo, ok := gitRepoName(r)
		if !ok {
			notFoundJSON(w, s)
			return
		}
		rep := s.GetRepo(owner, repo)
		if rep == nil {
			writeErr(w, s, Err(404, "Not Found"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			writeErr(w, s, Err(400, "Bad Request"))
			return
		}
		payloads, _ := parseAllPktLines(body)
		if len(payloads) == 0 {
			writeErr(w, s, Err(400, "Bad Request"))
			return
		}
		first := strings.TrimSuffix(string(payloads[0]), "\n")
		if strings.HasPrefix(first, "command=") {
			switch first[len("command="):] {
			case "ls-refs":
				handleLsRefs(w, s, rep, payloads[1:])
			case "fetch":
				handleFetchV2(w, s, rep, payloads[1:])
			default:
				writeErr(w, s, Err(400, "Bad Request"))
			}
			return
		}
		handleFetchV0(w, s, rep, payloads)
	}
}

// pushCommand is one receive-pack ref update.
type pushCommand struct {
	oldOID, newOID, ref string
}

func gitReceivePack(s *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.gitAuth(w, r) {
			return
		}
		owner, repo, ok := gitRepoName(r)
		if !ok {
			notFoundJSON(w, s)
			return
		}
		rep := s.GetRepo(owner, repo)
		if rep == nil {
			writeErr(w, s, Err(404, "Not Found"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			writeErr(w, s, Err(400, "Bad Request"))
			return
		}
		payloads, rest, _ := parsePktStream(body)
		if len(payloads) == 0 {
			writeErr(w, s, Err(400, "Bad Request"))
			return
		}
		var commands []pushCommand
		for i, p := range payloads {
			line := strings.TrimSuffix(string(p), "\n")
			if i == 0 {
				if nul := strings.IndexByte(line, 0); nul >= 0 {
					line = line[:nul]
				}
			}
			f := strings.Fields(line)
			if len(f) != 3 {
				writeErr(w, s, Err(400, "Bad Request"))
				return
			}
			commands = append(commands, pushCommand{oldOID: f[0], newOID: f[1], ref: f[2]})
		}
		pack := rest // the packfile follows the command flush (absent on delete-only pushes)

		s.mu.Lock()
		g := rep.Git
		var report bytes.Buffer
		unpackErr := ""
		if len(pack) > 0 {
			entries, err := decodePack(pack, func(oid string) *GitObject {
				return g.Objects[oid]
			})
			if err != nil {
				unpackErr = err.Error()
			} else {
				for _, e := range entries {
					g.WriteObject(e.typ, e.data)
				}
			}
		}
		if unpackErr != "" {
			report.Write(pktLineStr("unpack " + unpackErr + "\n"))
			for _, c := range commands {
				report.Write(pktLineStr("ng " + c.ref + " unpacker error\n"))
			}
		} else {
			report.Write(pktLineStr("unpack ok\n"))
			for _, c := range commands {
				cur, exists := g.Refs[c.ref]
				if c.newOID == zeroOID {
					// Delete.
					if exists && c.oldOID != zeroOID && c.oldOID != cur {
						report.Write(pktLineStr("ng " + c.ref + " non-fast-forward\n"))
						continue
					}
					if ok, reason := g.UpdateRef(c.ref, ""); !ok {
						report.Write(pktLineStr("ng " + c.ref + " " + reason + "\n"))
						continue
					}
					report.Write(pktLineStr("ok " + c.ref + "\n"))
					continue
				}
				if exists && c.oldOID == zeroOID {
					report.Write(pktLineStr("ng " + c.ref + " already exists\n"))
					continue
				}
				if c.oldOID != zeroOID && (!exists || c.oldOID != cur) {
					report.Write(pktLineStr("ng " + c.ref + " non-fast-forward\n"))
					continue
				}
				if _, ok := g.Objects[c.newOID]; !ok {
					report.Write(pktLineStr("ng " + c.ref + " object not found\n"))
					continue
				}
				if ok, reason := g.UpdateRef(c.ref, c.newOID); !ok {
					report.Write(pktLineStr("ng " + c.ref + " " + reason + "\n"))
					continue
				}
				report.Write(pktLineStr("ok " + c.ref + "\n"))
			}
		}
		report.Write(pktFlush)
		s.mu.Unlock()

		var out bytes.Buffer
		out.Write(sidebandWrap(report.Bytes()))
		out.Write(pktFlush)
		writeGitResponse(w, "application/x-git-receive-pack-result", out.Bytes())
	}
}

// gitDispatch routes git smart-HTTP requests without ServeMux patterns.
// The patterns GET /{owner}/{repo}/info/refs, POST .../git-upload-pack and
// POST .../git-receive-pack conflict with the REST /api/v3/{path...}
// fallback (both match paths like /api/v3/info/refs), and Go's ServeMux
// panics when conflicting patterns are registered. So main.go's "/"
// catch-all calls this instead; it returns true when the path is a git
// smart-HTTP path (handled, even on auth/404/405 errors). Path values are
// set with SetPathValue so gitRepoName keeps working unchanged.
func gitDispatch(w http.ResponseWriter, r *http.Request, s *Store) bool {
	segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segs) == 4 && segs[2] == "info" && segs[3] == "refs" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w, s)
			return true
		}
		r.SetPathValue("owner", segs[0])
		r.SetPathValue("repo", segs[1])
		gitInfoRefs(s)(w, r)
		return true
	}
	if len(segs) == 3 && (segs[2] == "git-upload-pack" || segs[2] == "git-receive-pack") {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, s)
			return true
		}
		r.SetPathValue("owner", segs[0])
		r.SetPathValue("repo", segs[1])
		if segs[2] == "git-upload-pack" {
			gitUploadPack(s)(w, r)
		} else {
			gitReceivePack(s)(w, r)
		}
		return true
	}
	return false
}
