// scenario.go: the contest's fixed seeded scenario (port of the suite's wire.ts RNG) and the
// forge state — repositories as in-memory git object stores, issues, pulls, labels, merge.
package main

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const u64Mask = ^uint64(0)

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}
var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func fnv1a64(text string) uint64 {
	hash := uint64(0xcbf29ce484222325)
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash *= 0x100000001b3
		hash &= u64Mask
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64Mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64Mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64Mask
	return (z ^ (z >> 31)) & u64Mask
}

const scenarioSeed uint64 = 0x6a09e667f3bcc909

func scenarioSeedFor(label string) uint64 {
	return mix64(scenarioSeed ^ fnv1a64(label))
}

type Rng struct{ state uint64 }

func NewRng(seed uint64) *Rng {
	if seed == 0 {
		seed = scenarioSeed
	}
	return &Rng{state: seed}
}

func (r *Rng) NextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64Mask
	x = (x ^ (x << 25)) & u64Mask
	x = (x ^ (x >> 27)) & u64Mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64Mask
}

func (r *Rng) NextU32() uint32 {
	return uint32(r.NextU64() >> 32)
}

func (r *Rng) NextInt(maxExclusive int) int {
	if maxExclusive == 1 {
		return 0
	}
	rangeU := uint64(4294967296)
	limit := rangeU - rangeU%uint64(maxExclusive)
	for {
		v := uint64(r.NextU32())
		if v < limit {
			return int(v % uint64(maxExclusive))
		}
	}
}

func (r *Rng) Pick(items []string) string {
	return items[r.NextInt(len(items))]
}

func GenerateUsers(count int) []struct{ ID int; Login string } {
	rng := NewRng(scenarioSeedFor("users"))
	used := map[string]bool{}
	users := make([]struct{ ID int; Login string }, 0, count)
	for i := 0; i < count; i++ {
		first := rng.Pick(firstNames)
		last := rng.Pick(lastNames)
		base := sanitizeLogin(first + last)
		login := base
		suffix := 2
		for used[login] {
			login = fmt.Sprintf("%s-%d", base, suffix)
			suffix++
		}
		used[login] = true
		users = append(users, struct{ ID int; Login string }{1000 + i, login})
	}
	return users
}

func sanitizeLogin(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

var users = GenerateUsers(12)
var ViewerLogin = users[0].Login
var ViewerID = users[0].ID

// ---------------------------------------------------------------- forge state

var epochCounter int64

func NowISO() string {
	c := atomic.AddInt64(&epochCounter, 1)
	return time.Unix(1735689600+c, 0).UTC().Format("2006-01-02T15:04:05Z")
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []string
	Comments  []Comment
	CreatedAt string
	ClosedAt  string
	IsPull    bool
}

type Comment struct {
	ID        int
	Body      string
	Login     string
	CreatedAt string
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string
	HeadRef        string
	HeadSha        string
	BaseRef        string
	BaseSha        string
	Merged         bool
	MergedAt       string
	MergeCommitSha string
	MergedBy       string
	CreatedAt      string
}

type Repo struct {
	ID              int
	Name            string
	Owner           string
	Description     string
	Private         bool
	CreatedAt       string
	UpdatedAt       string
	DefaultBranch   string
	Objects         map[string]GitObject
	Refs            map[string]string
	Issues          map[int]*Issue
	Pulls           map[int]*Pull
	Labels          map[string]string
	NextIssueNumber int
	NextCommentID   int
}

type AlreadyExists struct{}

func (AlreadyExists) Error() string { return "name already exists on this account" }

type Forge struct {
	mu         syncMap
	repos      map[string]*Repo
	nextRepoID int
}

// syncMap is a plain mutex wrapper; the suite is single-threaded per check group but the
// benchmark fires concurrent clones, so guard the repo map only.
type syncMap struct{ inner map[string]*Repo }

func NewForge() *Forge {
	return &Forge{repos: map[string]*Repo{}, nextRepoID: 5001}
}

func (f *Forge) repoKey(owner, name string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(name)
}

func (f *Forge) GetRepo(owner, name string) *Repo {
	return f.repos[f.repoKey(owner, name)]
}

func (f *Forge) CreateRepo(name, description string, isPrivate bool, owner string) (*Repo, error) {
	key := f.repoKey(owner, name)
	if _, ok := f.repos[key]; ok {
		return nil, AlreadyExists{}
	}
	repo := &Repo{
		ID: f.nextRepoID, Name: name, Owner: owner, Description: description,
		Private: isPrivate, CreatedAt: NowISO(), UpdatedAt: NowISO(),
		DefaultBranch: "main", Objects: map[string]GitObject{}, Refs: map[string]string{},
		Issues: map[int]*Issue{}, Pulls: map[int]*Pull{}, Labels: map[string]string{},
		NextIssueNumber: 1, NextCommentID: 1,
	}
	f.repos[key] = repo
	f.nextRepoID++
	return repo, nil
}

func (f *Forge) PutObject(repo *Repo, t ObjectType, data []byte) GitObject {
	oid := HashObject(t, data)
	if o, ok := repo.Objects[oid]; ok {
		return o
	}
	o := GitObject{OID: oid, Type: t, Data: data}
	repo.Objects[oid] = o
	return o
}

func (f *Forge) WriteTreeFromFiles(repo *Repo, files map[string]FileEntry) string {
	byDir := map[string]map[string]FileEntry{}
	for path, entry := range files {
		if entry.Type == "tree" {
			continue // directories are rebuilt from blob paths
		}
		idx := strings.LastIndex(path, "/")
		dir, base := "", path
		if idx >= 0 {
			dir = path[:idx]
			base = path[idx+1:]
		}
		if byDir[dir] == nil {
			byDir[dir] = map[string]FileEntry{}
		}
		byDir[dir][base] = entry
	}
	var build func(dir string) string
	build = func(dir string) string {
		var entries []TreeEntry
		for name, entry := range byDir[dir] {
			entries = append(entries, TreeEntry{Mode: entry.Mode, Name: name, OID: entry.OID, Type: entry.Type})
		}
		prefix := ""
		if dir != "" {
			prefix = dir + "/"
		}
		seenDirs := map[string]bool{}
		for path := range files {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			rest := path[len(prefix):]
			slash := strings.Index(rest, "/")
			if slash >= 0 {
				sub := rest[:slash]
				if !seenDirs[sub] {
					seenDirs[sub] = true
					var subDir string
					if dir == "" {
						subDir = sub
					} else {
						subDir = prefix + sub
					}
					subOid := build(subDir)
					entries = append(entries, TreeEntry{Mode: "40000", Name: sub, OID: subOid, Type: "tree"})
				}
			}
		}
		return f.PutObject(repo, ObjTree, BuildTree(entries)).OID
	}
	return build("")
}

type FileEntry struct {
	OID  string
	Mode string
	Type string
}

func (f *Forge) FilesAtTree(repo *Repo, treeOID, prefix string, out map[string]FileEntry) map[string]FileEntry {
	if out == nil {
		out = map[string]FileEntry{}
	}
	object, ok := repo.Objects[treeOID]
	if !ok || object.Type != ObjTree {
		return out
	}
	for _, e := range ParseTree(object.Data) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if e.Type == "tree" {
			out[path] = FileEntry{OID: e.OID, Mode: e.Mode, Type: "tree"}
			f.FilesAtTree(repo, e.OID, path, out)
		} else {
			out[path] = FileEntry{OID: e.OID, Mode: e.Mode, Type: "blob"}
		}
	}
	return out
}

func (f *Forge) CommitView(repo *Repo, oid string) *CommitView {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != ObjCommit {
		return nil
	}
	v := ParseCommit(object.Data)
	return &v
}

func (f *Forge) HeadCommit(repo *Repo, branch string) (string, bool) {
	if branch == "" {
		branch = repo.DefaultBranch
	}
	oid, ok := repo.Refs["refs/heads/"+branch]
	if ok && oid != ZeroOID {
		return oid, true
	}
	return "", false
}

func (f *Forge) IsAncestor(repo *Repo, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{}
	queue := []string{descendant}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if oid == ancestor {
			return true
		}
		if v := f.CommitView(repo, oid); v != nil {
			queue = append(queue, v.Parents...)
		}
	}
	return false
}

func (f *Forge) MergeBase(repo *Repo, a, b string) string {
	ancestors := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ancestors[oid] {
			continue
		}
		ancestors[oid] = true
		if v := f.CommitView(repo, oid); v != nil {
			queue = append(queue, v.Parents...)
		}
	}
	seen := map[string]bool{}
	q := []string{b}
	for len(q) > 0 {
		oid := q[0]
		q = q[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancestors[oid] {
			return oid
		}
		if v := f.CommitView(repo, oid); v != nil {
			q = append(q, v.Parents...)
		}
	}
	return ""
}

func (f *Forge) CommitFiles(repo *Repo, files map[string]FileEntry, parent string, message, author string) string {
	treeOid := f.WriteTreeFromFiles(repo, files)
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	commit := BuildCommit(BuildCommitFields{
		Tree: treeOid, Parents: parents, AuthorName: author, CommitterName: author, Message: message,
	})
	return f.PutObject(repo, ObjCommit, commit).OID
}

func (f *Forge) DiffTrees(repo *Repo, baseFiles, headFiles map[string]FileEntry) []DiffRow {
	var rows []DiffRow
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, path := range sorted {
		b, hasB := baseFiles[path]
		h, hasH := headFiles[path]
		if (hasB && b.Type == "tree") || (hasH && h.Type == "tree") {
			continue
		}
		if hasB && !hasH {
			lines := 0
			if blob, ok := repo.Objects[b.OID]; ok {
				lines = countLines(blob.Data)
			}
			rows = append(rows, DiffRow{Filename: path, Status: "removed", Deletions: lines, Changes: lines})
		} else if !hasB && hasH {
			lines := 0
			if blob, ok := repo.Objects[h.OID]; ok {
				lines = countLines(blob.Data)
			}
			rows = append(rows, DiffRow{Filename: path, Status: "added", Additions: lines, Changes: lines})
		} else if hasB && hasH && b.OID != h.OID {
			var bd, hd []byte
			if blob, ok := repo.Objects[b.OID]; ok {
				bd = blob.Data
			}
			if blob, ok := repo.Objects[h.OID]; ok {
				hd = blob.Data
			}
			additions, deletions := DiffLineCounts(bd, hd)
			rows = append(rows, DiffRow{Filename: path, Status: "modified", Additions: additions, Deletions: deletions, Changes: additions + deletions})
		}
	}
	return rows
}

type DiffRow struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

func countLines(data []byte) int {
	lines := 0
	for _, b := range data {
		if b == 0x0a {
			lines++
		}
	}
	if len(data) > 0 && data[len(data)-1] != 0x0a {
		lines++
	}
	return lines
}

func DiffLineCounts(a, b []byte) (int, int) {
	countOf := func(buf []byte) map[string]int {
		m := map[string]int{}
		for _, line := range strings.Split(string(buf), "\n") {
			m[line]++
		}
		return m
	}
	am, bm := countOf(a), countOf(b)
	additions, deletions := 0, 0
	for line, n := range bm {
		if have := am[line]; n > have {
			additions += n - have
		}
	}
	for line, n := range am {
		if have := bm[line]; n > have {
			deletions += n - have
		}
	}
	return additions, deletions
}

type MergeResult struct {
	OK      bool
	SHA     string
	Message string
	Status  int
}

func (f *Forge) MergePull(repo *Repo, pull *Pull, method string) MergeResult {
	if pull.Merged {
		return MergeResult{OK: false, Status: 405, Message: "Pull Request is already merged"}
	}
	if pull.State != "open" {
		return MergeResult{OK: false, Status: 405, Message: fmt.Sprintf("Pull Request %d is closed and cannot be merged", pull.Number)}
	}
	baseTip, ok1 := repo.Refs["refs/heads/"+pull.BaseRef]
	headTip, ok2 := repo.Refs["refs/heads/"+pull.HeadRef]
	if !ok1 || headTip == "" {
		headTip = pull.HeadSha
		ok2 = repo.Objects[headTip].Data != nil
	}
	if !ok1 || !ok2 {
		return MergeResult{OK: false, Status: 422, Message: "Pull Request branch is gone"}
	}
	baseView := f.CommitView(repo, baseTip)
	headView := f.CommitView(repo, headTip)
	if baseView == nil || headView == nil {
		return MergeResult{OK: false, Status: 422, Message: "Pull Request commits are missing"}
	}
	baseFiles := f.FilesAtTree(repo, baseView.Tree, "", nil)
	headFiles := f.FilesAtTree(repo, headView.Tree, "", nil)
	mb := f.MergeBase(repo, baseTip, headTip)
	mbFiles := map[string]FileEntry{}
	if mb != "" {
		if mbView := f.CommitView(repo, mb); mbView != nil {
			mbFiles = f.FilesAtTree(repo, mbView.Tree, "", nil)
		}
	}
	baseChanged := map[string]bool{}
	for path, entry := range baseFiles {
		if entry.Type == "tree" {
			continue
		}
		before, ok := mbFiles[path]
		if !ok || before.OID != entry.OID {
			baseChanged[path] = true
		}
	}
	headChanged := map[string]bool{}
	for path, entry := range headFiles {
		if entry.Type == "tree" {
			continue
		}
		before, ok := mbFiles[path]
		if !ok || before.OID != entry.OID {
			headChanged[path] = true
		}
	}
	for path := range baseChanged {
		if headChanged[path] {
			b := baseFiles[path].OID
			h := headFiles[path].OID
			m := mbFiles[path].OID
			if b != m && h != m {
				return MergeResult{OK: false, Status: 405, Message: "Pull Request is not mergeable: conflicts"}
			}
		}
	}
	files := map[string]FileEntry{}
	for k, v := range headFiles {
		files[k] = v
	}
	message := fmt.Sprintf("%s (#%d)", pull.Title, pull.Number)
	sha := f.CommitFiles(repo, files, baseTip, message, pull.MergedBy)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	pull.MergedAt = NowISO()
	pull.MergeCommitSha = sha
	return MergeResult{OK: true, SHA: sha, Message: "Pull Request successfully merged"}
}
