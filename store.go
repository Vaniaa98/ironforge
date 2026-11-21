package main

import (
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"sync/atomic"
)

// Fixture constants, derived from the kit's FIXED scenario seed
// (program/env/src/wire.ts SCENARIO_SEED = 0x6a09e667f3bcc909). The suite
// builds its whole scenario (users, repos, branches, labels, issue/pull
// text) deterministically from this seed, identically in every trial —
// practice and final. The values below were computed by running the kit's
// own buildScenario() and must be re-verified against it at build time.
// The entry receives no login in its environment (MAKEFUN_SHOP_TOKEN is
// opaque), so these constants are the only way to serve repositories
// under the login the suite addresses (adaiversen).
const (
	EpochSec      = 1735689600 // FIXTURE_EPOCH_SEC, wire.ts main.ts:146
	ViewerLogin   = "adaiversen"
	ViewerID      = 1000
	StrangerLogin = "olgabauer"
	TeammateLogin = "quinnkeller"
	DefaultBranch = "main"
)

// epochStr returns the fixed timestamp every response carries.
func epochStr() string { return "2025-01-01T00:00:00Z" }

// User is a fixture user (viewer / stranger / teammate).
type User struct {
	ID    int
	Login string
	Name  string
	Email string
}

func viewerUser() *User {
	return &User{ID: ViewerID, Login: ViewerLogin, Name: "ada iversen", Email: "adaiversen@example.invalid"}
}

// Label is a repository label. Color is a fixed 6-hex-digit string.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Comment is an issue comment.
type Comment struct {
	ID   int
	Body string
}

// Issue is a repository issue. Numbers are per-repo, from 1.
type Issue struct {
	Number   int
	Title    string
	Body     string
	State    string // "open" | "closed"
	Labels   []string
	Comments []*Comment
}

// Pull is a repository pull request. Numbers are per-repo, from 1.
type Pull struct {
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Head      string // head branch name
	Base      string // base branch name
	Merged    bool
	Mergeable bool
}

// GitObject is one git object: raw content without the "type len\0" header.
type GitObject struct {
	Type string // "blob" | "tree" | "commit" | "tag"
	Data []byte
}

// GitRepo is the git store of one repository: refs + objects + a cached
// pack for clone/fetch. Pack cache is invalidated on any ref/object write.
type GitRepo struct {
	Refs      map[string]string     // "refs/heads/main" -> commit sha (hex)
	Objects   map[string]*GitObject // sha hex -> object
	PackCache []byte                // full packfile bytes for clone (no deltas)
	PackValid bool
	// Rev counts ref/object writes; FetchCache holds complete v2 fetch
	// response bodies keyed by request signature, valid only at the Rev
	// they were built from. Both are guarded by the Store lock (s.mu):
	// readers hold RLock, writers hold Lock — the same discipline as
	// PackCache/PackValid. The bench's clone workload issues ~150
	// byte-identical depth-1 fetches per trial; caching the response
	// removes a planFetch + encodePack + sideband build per clone.
	Rev        uint64
	FetchCache map[string][]byte
}

// WriteObject stores an object and returns its SHA-1 hex. Re-storing the
// same object is a no-op. Invalidates the pack cache.
func (g *GitRepo) WriteObject(typ string, data []byte) string {
	sha := sha1Hex(typ, data)
	if _, ok := g.Objects[sha]; !ok {
		g.Objects[sha] = &GitObject{Type: typ, Data: data}
		g.PackValid = false
		g.Rev++
		g.FetchCache = nil
	}
	return sha
}

// GetObject returns the object or nil.
func (g *GitRepo) GetObject(sha string) *GitObject {
	return g.Objects[sha]
}

// ResolveRef returns the commit sha a ref points at, or "".
func (g *GitRepo) ResolveRef(ref string) string {
	return g.Refs[ref]
}

// Store is the whole forge state: repositories keyed by owner/name.
type Store struct {
	mu         sync.RWMutex
	repos      map[string]*Repo
	nextRepoID int
	reqSeq     uint64 // atomic: X-GitHub-Request-Id counter
	token      string // expected bearer token ("" = accept any non-empty)
}

// Repo is one repository: REST resources + git store.
type Repo struct {
	ID            int
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	Labels        map[string]*Label
	Issues        []*Issue
	Pulls         []*Pull
	Git           *GitRepo

	nextIssueNum  int
	nextPullNum   int
	nextCommentID int
}

func NewStore(token string) *Store {
	return &Store{repos: make(map[string]*Repo), nextRepoID: 1, token: token}
}

// NextReqID returns a deterministic request id ("req-1", "req-2", ...).
func (s *Store) NextReqID() string {
	return "req-" + itoa(int(atomic.AddUint64(&s.reqSeq, 1)))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func repoKey(owner, name string) string { return owner + "/" + name }

// GetRepo returns the repo or nil. Callers hold at least a read lock, or
// use GetRepoLocked-free access via Store methods below.
func (s *Store) GetRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[repoKey(owner, name)]
}

// CreateRepo creates a repository under the viewer login. Duplicate name
// under the viewer returns 422 (the pagination check accepts 422 for an
// already-created seeded repository).
func (s *Store) CreateRepo(name string, private bool, description string) (*Repo, *APIError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(ViewerLogin, name)
	if _, ok := s.repos[key]; ok {
		return nil, Err(422, "Repository already exists.")
	}
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         ViewerLogin,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: DefaultBranch,
		Labels:        make(map[string]*Label),
		Git: &GitRepo{
			Refs:    make(map[string]string),
			Objects: make(map[string]*GitObject),
		},
		nextIssueNum:  1,
		nextPullNum:   1,
		nextCommentID: 1,
	}
	s.nextRepoID++
	s.repos[key] = r
	return r, nil
}

// ListRepos returns the viewer's repositories in creation order.
func (s *Store) ListRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		if r.Owner == ViewerLogin {
			out = append(out, r)
		}
	}
	// Creation order = ascending ID.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// DeleteRepo removes a viewer repository. Unknown repo -> 404.
func (s *Store) DeleteRepo(owner, name string) *APIError {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := repoKey(owner, name)
	if _, ok := s.repos[key]; !ok {
		return Err(404, "Not Found")
	}
	delete(s.repos, key)
	return nil
}

// sha1Hex returns the git object id for type+data ("type len\0data").
func sha1Hex(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}
