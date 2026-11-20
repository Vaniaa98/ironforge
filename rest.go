package main

// REST v3 API — worker 1 (REST + protocol).
//
// Implements the 50 checks in program/env/src/checks-rest.ts (repos 15,
// issues 15, pulls 20, protocol 5) plus the REST endpoints the MCP checks
// lean on (GET /api/v3/user, GET /git/refs/heads/{branch},
// GET /branches/{branch}, implicit branch creation via PUT /contents).
//
// Locking: every handler runs under the store write lock (restLock), taken
// through restRepoLocked / directly, and accesses s.repos + Repo/GitRepo
// fields directly. The Store's own methods (GetRepo/CreateRepo/...) lock
// internally and must NOT be called while restLock is held (deadlock).
// createPullInRepo is the one shared entry point for the GraphQL worker:
// it takes the lock itself, so callers must NOT hold it when calling it;
// REST handlers use the lock-held twin createPullLocked instead.
//
// Git dependence: commit objects are parsed as standard git commit objects
// ("tree <sha>", "parent <sha>" lines, blank line, message) via
// restParseCommit, using only GitRepo.GetObject. Tree walks use the worker-2
// helpers GetFileAtCommit / ListDirAtCommit / WriteBlob / WriteTree /
// WriteCommit / UpdateRef. No git helper is defined here; the stub file
// zz-stub-githelpers.go (deleted at merge time) provides them for this
// copy so it compiles standalone.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Lock helpers (Store.mu is private; same-package methods may touch it).
// ---------------------------------------------------------------------------

func (s *Store) restLock()   { s.mu.Lock() }
func (s *Store) restUnlock() { s.mu.Unlock() }

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

func registerREST(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("/api/v3/{$}", func(w http.ResponseWriter, r *http.Request) { s.handleRoot(w, r) })
	mux.HandleFunc("/api/v3/meta", func(w http.ResponseWriter, r *http.Request) { s.handleMeta(w, r) })
	mux.HandleFunc("/api/v3/user", func(w http.ResponseWriter, r *http.Request) { s.handleUser(w, r) })
	mux.HandleFunc("/api/v3/user/repos", func(w http.ResponseWriter, r *http.Request) { s.handleUserRepos(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) { s.handleRepo(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/branches", func(w http.ResponseWriter, r *http.Request) { s.handleBranches(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/branches/{branch...}", func(w http.ResponseWriter, r *http.Request) { s.handleBranch(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/commits", func(w http.ResponseWriter, r *http.Request) { s.handleCommits(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/git/refs", func(w http.ResponseWriter, r *http.Request) { s.handleGitRefs(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) { s.handleGitRefHead(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/contents", func(w http.ResponseWriter, r *http.Request) { s.handleContents(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) { s.handleContents(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) { s.handleLabels(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) { s.handleIssues(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/issues/{num}", func(w http.ResponseWriter, r *http.Request) { s.handleIssue(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/issues/{num}/comments", func(w http.ResponseWriter, r *http.Request) { s.handleIssueComments(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) { s.handlePulls(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/pulls/{num}", func(w http.ResponseWriter, r *http.Request) { s.handlePull(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/pulls/{num}/files", func(w http.ResponseWriter, r *http.Request) { s.handlePullFiles(w, r) })
	mux.HandleFunc("/api/v3/repos/{owner}/{repo}/pulls/{num}/merge", func(w http.ResponseWriter, r *http.Request) { s.handlePullMerge(w, r) })
	// Unknown /api/v3 route: auth first (401 without a token), else JSON 404.
	mux.HandleFunc("/api/v3/{path...}", func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAuth(w, r) {
			return
		}
		notFoundJSON(w, s)
	})
}

// ---------------------------------------------------------------------------
// Response views (lean structs; extra fields are allowed by the checks).
// ---------------------------------------------------------------------------

type restUserView struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Type  string `json:"type"`
}

func restViewerView() restUserView {
	return restUserView{Login: ViewerLogin, ID: ViewerID, Name: "ada iversen", Email: "adaiversen@example.invalid", Type: "User"}
}

type restRepoView struct {
	ID            int          `json:"id"`
	Name          string       `json:"name"`
	FullName      string       `json:"full_name"`
	Owner         restUserView `json:"owner"`
	Private       bool         `json:"private"`
	Description   string       `json:"description"`
	DefaultBranch string       `json:"default_branch"`
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
	Fork          bool         `json:"fork"`
	HasIssues     bool         `json:"has_issues"`
}

func restRepoViewOf(r *Repo) restRepoView {
	return restRepoView{
		ID: r.ID, Name: r.Name, FullName: r.Owner + "/" + r.Name,
		Owner:   restUserView{Login: r.Owner, ID: ViewerID, Type: "User"},
		Private: r.Private, Description: r.Description, DefaultBranch: r.DefaultBranch,
		CreatedAt: epochStr(), UpdatedAt: epochStr(), Fork: false, HasIssues: true,
	}
}

type restLabelView struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

func restLabelViewOf(l *Label) restLabelView {
	return restLabelView{Name: l.Name, Color: l.Color, Description: l.Description}
}

type restIssueView struct {
	ID        int             `json:"id"`
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	State     string          `json:"state"`
	Labels    []restLabelView `json:"labels"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	User      restUserView    `json:"user"`
}

func restIssueViewOf(repo *Repo, iss *Issue) restIssueView {
	labels := make([]restLabelView, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		if l := repo.Labels[name]; l != nil {
			labels = append(labels, restLabelViewOf(l))
		} else {
			labels = append(labels, restLabelView{Name: name})
		}
	}
	return restIssueView{
		ID: iss.Number, Number: iss.Number, Title: iss.Title, Body: iss.Body,
		State: iss.State, Labels: labels,
		CreatedAt: epochStr(), UpdatedAt: epochStr(), User: restViewerView(),
	}
}

type restCommentView struct {
	ID        int          `json:"id"`
	Body      string       `json:"body"`
	CreatedAt string       `json:"created_at"`
	UpdatedAt string       `json:"updated_at"`
	User      restUserView `json:"user"`
}

type restCommitRef struct {
	SHA string `json:"sha"`
}

type restBranchView struct {
	Name   string        `json:"name"`
	Commit restCommitRef `json:"commit"`
}

type restAuthorView struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	Date  string `json:"date"`
}

type restCommitDetail struct {
	Message   string         `json:"message"`
	Author    restAuthorView `json:"author"`
	Committer restAuthorView `json:"committer"`
}

type restCommitView struct {
	SHA    string           `json:"sha"`
	Commit restCommitDetail `json:"commit"`
}

type restRefView struct {
	Ref    string        `json:"ref"`
	Object restCommitRef `json:"object"`
}

type restContentFileView struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
	Size     int    `json:"size"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

type restContentEntryView struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Size int    `json:"size"`
}

type restBranchRefView struct {
	Ref   string `json:"ref"`
	SHA   string `json:"sha"`
	Label string `json:"label"`
}

type restPullView struct {
	ID             int               `json:"id"`
	Number         int               `json:"number"`
	Title          string            `json:"title"`
	Body           string            `json:"body"`
	State          string            `json:"state"`
	Head           restBranchRefView `json:"head"`
	Base           restBranchRefView `json:"base"`
	Merged         bool              `json:"merged"`
	Mergeable      bool              `json:"mergeable"`
	MergedAt       any               `json:"merged_at"`
	MergeCommitSHA any               `json:"merge_commit_sha"`
	CreatedAt      string            `json:"created_at"`
	UpdatedAt      string            `json:"updated_at"`
	User           restUserView      `json:"user"`
}

type restPullFileView struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// ---------------------------------------------------------------------------
// Small helpers
// ---------------------------------------------------------------------------

// writeNoContent answers with headers but no body (e.g. DELETE repo -> 204).
func writeNoContent(w http.ResponseWriter, s *Store, status int) {
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("X-GitHub-Request-Id", s.NextReqID())
	w.WriteHeader(status)
}

// restRepoLocked authenticates, resolves the repo under the viewer login and
// takes the store write lock. Caller MUST defer s.restUnlock() on success.
// Any other owner is 404 (never 403); a missing repo is 404.
func (s *Store) restRepoLocked(w http.ResponseWriter, r *http.Request) (*Repo, bool) {
	if !s.requireAuth(w, r) {
		return nil, false
	}
	owner := r.PathValue("owner")
	name := r.PathValue("repo")
	if owner != ViewerLogin {
		writeErr(w, s, Err(404, "Not Found"))
		return nil, false
	}
	s.restLock()
	repo := s.repos[repoKey(owner, name)]
	if repo == nil {
		s.restUnlock()
		writeErr(w, s, Err(404, "Not Found"))
		return nil, false
	}
	return repo, true
}

func restParsePageParams(r *http.Request) (perPage, page int) {
	perPage, page = 30, 1
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	return perPage, page
}

func restSetNextLink(w http.ResponseWriter, r *http.Request, path string, perPage, page, total int) {
	if page*perPage >= total {
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	next := fmt.Sprintf("%s://%s%s?per_page=%d&page=%d", scheme, r.Host, path, perPage, page+1)
	w.Header().Set("Link", "<"+next+">; rel=\"next\"")
}

func restParseNum(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func restBaseName(path string) string {
	path = strings.TrimSuffix(path, "/")
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func restCleanB64(s string) string {
	return strings.NewReplacer("\n", "", "\r", "", " ", "", "\t", "").Replace(s)
}

// ---------------------------------------------------------------------------
// Root / meta / user
// ---------------------------------------------------------------------------

func (s *Store) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo, user")
	writeJSON(w, s, 200, map[string]string{
		"current_user_url": "https://api.github.com/user",
		"repository_url":   "https://api.github.com/repos/{owner}/{repo}",
		"rate_limit_url":   "https://api.github.com/rate_limit",
	})
}

func (s *Store) handleMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	writeJSON(w, s, 200, struct {
		InstalledVersion string `json:"installed_version"`
	}{InstalledVersion: "1.0.0"})
}

func (s *Store) handleUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	if !s.requireAuth(w, r) {
		return
	}
	writeJSON(w, s, 200, restViewerView())
}

// ---------------------------------------------------------------------------
// /user/repos + /repos/{o}/{r}
// ---------------------------------------------------------------------------

func (s *Store) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		if !s.requireAuth(w, r) {
			return
		}
		perPage, page := restParsePageParams(r)
		repos := s.ListRepos()
		total := len(repos)
		start := (page - 1) * perPage
		if start > total {
			start = total
		}
		end := start + perPage
		if end > total {
			end = total
		}
		out := make([]restRepoView, 0, end-start)
		for _, repo := range repos[start:end] {
			out = append(out, restRepoViewOf(repo))
		}
		restSetNextLink(w, r, "/api/v3/user/repos", perPage, page, total)
		writeJSON(w, s, 200, out)
	case http.MethodPost:
		if !s.requireAuth(w, r) {
			return
		}
		var body struct {
			Name          string `json:"name"`
			Private       bool   `json:"private"`
			Description   string `json:"description"`
			DefaultBranch string `json:"default_branch"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeErr(w, s, Err(422, "Repository creation failed."))
			return
		}
		defBranch := body.DefaultBranch
		if defBranch == "" {
			defBranch = DefaultBranch
		}
		s.restLock()
		key := repoKey(ViewerLogin, body.Name)
		if _, ok := s.repos[key]; ok {
			s.restUnlock()
			writeErr(w, s, Err(422, "Repository already exists."))
			return
		}
		repo := &Repo{
			ID: s.nextRepoID, Owner: ViewerLogin, Name: body.Name,
			Private: body.Private, Description: body.Description, DefaultBranch: defBranch,
			Labels:       make(map[string]*Label),
			Git:          &GitRepo{Refs: make(map[string]string), Objects: make(map[string]*GitObject)},
			nextIssueNum: 1, nextPullNum: 1, nextCommentID: 1,
		}
		s.nextRepoID++
		s.repos[key] = repo
		view := restRepoViewOf(repo)
		s.restUnlock()
		writeJSON(w, s, 201, view)
	default:
		methodNotAllowed(w, s)
	}
}

func (s *Store) handleRepo(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s, 200, restRepoViewOf(repo))
	case http.MethodPatch:
		var body struct {
			Description *string `json:"description"`
			Name        *string `json:"name"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		if body.Description != nil {
			repo.Description = *body.Description
		}
		if body.Name != nil && *body.Name != "" && *body.Name != repo.Name {
			newKey := repoKey(repo.Owner, *body.Name)
			if _, exists := s.repos[newKey]; exists {
				writeErr(w, s, Err(422, "Repository already exists."))
				return
			}
			delete(s.repos, repoKey(repo.Owner, repo.Name))
			repo.Name = *body.Name
			s.repos[newKey] = repo
		}
		writeJSON(w, s, 200, restRepoViewOf(repo))
	case http.MethodDelete:
		delete(s.repos, repoKey(repo.Owner, repo.Name))
		writeNoContent(w, s, 204)
	default:
		methodNotAllowed(w, s)
	}
}

// ---------------------------------------------------------------------------
// Branches + commits
// ---------------------------------------------------------------------------

func (s *Store) handleBranches(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	names := make([]string, 0, len(repo.Git.Refs))
	for ref := range repo.Git.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	out := make([]restBranchView, 0, len(names))
	for _, name := range names {
		out = append(out, restBranchView{Name: name, Commit: restCommitRef{SHA: repo.Git.Refs["refs/heads/"+name]}})
	}
	writeJSON(w, s, 200, out)
}

func (s *Store) handleBranch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	branch := r.PathValue("branch")
	sha := repo.Git.ResolveRef("refs/heads/" + branch)
	if sha == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	writeJSON(w, s, 200, restBranchView{Name: branch, Commit: restCommitRef{SHA: sha}})
}

// restResolveCommitRef turns a ?sha= / ?ref= value into a commit sha:
// a branch name, a full ref, or a raw 40-hex commit id. "" = default tip.
func restResolveCommitRef(g *GitRepo, repo *Repo, ref string) string {
	if ref == "" {
		return g.ResolveRef("refs/heads/" + repo.DefaultBranch)
	}
	if sha := g.ResolveRef("refs/heads/" + ref); sha != "" {
		return sha
	}
	if sha := g.ResolveRef(ref); sha != "" {
		return sha
	}
	if len(ref) == 40 {
		if obj := g.GetObject(ref); obj != nil && obj.Type == "commit" {
			return ref
		}
	}
	return ""
}

func (s *Store) handleCommits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	tip := restResolveCommitRef(repo.Git, repo, r.URL.Query().Get("sha"))
	if tip == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	out := make([]restCommitView, 0, 8)
	seen := make(map[string]bool)
	queue := []string{tip}
	for len(queue) > 0 {
		sha := queue[0]
		queue = queue[1:]
		if sha == "" || seen[sha] {
			continue
		}
		seen[sha] = true
		obj := repo.Git.GetObject(sha)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents, msg, aName, aEmail := restParseCommit(obj.Data)
		author := restAuthorView{Name: aName, Email: aEmail, Date: epochStr()}
		if author.Name == "" {
			author.Name = "ada iversen"
			author.Email = "adaiversen@example.invalid"
		}
		out = append(out, restCommitView{SHA: sha, Commit: restCommitDetail{Message: msg, Author: author, Committer: author}})
		queue = append(queue, parents...)
	}
	writeJSON(w, s, 200, out)
}

// ---------------------------------------------------------------------------
// Git Data API: POST /git/refs, GET /git/refs/heads/{branch}
// ---------------------------------------------------------------------------

func (s *Store) handleGitRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	var body struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, s, err)
		return
	}
	if !strings.HasPrefix(body.Ref, "refs/heads/") || len(body.Ref) <= len("refs/heads/") {
		writeErr(w, s, Err(422, "Reference creation failed."))
		return
	}
	if _, exists := repo.Git.Refs[body.Ref]; exists {
		writeErr(w, s, Err(422, "Reference already exists."))
		return
	}
	obj := repo.Git.GetObject(body.SHA)
	if obj == nil || obj.Type != "commit" {
		writeErr(w, s, Err(422, "Reference creation failed."))
		return
	}
	if ok, _ := repo.Git.UpdateRef(body.Ref, body.SHA); !ok {
		writeErr(w, s, Err(422, "Reference creation failed."))
		return
	}
	writeJSON(w, s, 201, restRefView{Ref: body.Ref, Object: restCommitRef{SHA: body.SHA}})
}

func (s *Store) handleGitRefHead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	branch := r.PathValue("branch")
	sha := repo.Git.ResolveRef("refs/heads/" + branch)
	if sha == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	writeJSON(w, s, 200, restRefView{Ref: "refs/heads/" + branch, Object: restCommitRef{SHA: sha}})
}

// ---------------------------------------------------------------------------
// Contents (files live in the default branch's git tree; every write is one
// commit). PUT 201 create / 200 update / 409 stale sha; GET file (base64)
// or directory listing; DELETE 200 {commit:{sha}}.
// ---------------------------------------------------------------------------

func (s *Store) handleContents(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	path := strings.TrimPrefix(r.PathValue("path"), "/")
	switch r.Method {
	case http.MethodGet:
		s.restContentsGet(w, repo, path, r.URL.Query().Get("ref"))
	case http.MethodPut:
		s.restContentsPut(w, r, repo, path)
	case http.MethodDelete:
		s.restContentsDelete(w, r, repo, path)
	default:
		methodNotAllowed(w, s)
	}
}

func (s *Store) restContentsGet(w http.ResponseWriter, repo *Repo, path, ref string) {
	tip := restResolveCommitRef(repo.Git, repo, ref)
	if tip == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	if data, found := repo.Git.GetFileAtCommit(tip, path); found {
		writeJSON(w, s, 200, restContentFileView{
			Type: "file", Encoding: "base64",
			Content: base64.StdEncoding.EncodeToString(data),
			SHA:     sha1Hex("blob", data), Size: len(data),
			Name: restBaseName(path), Path: path,
		})
		return
	}
	entries := repo.Git.ListDirAtCommit(tip, path)
	if len(entries) == 0 {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	out := make([]restContentEntryView, 0, len(entries))
	for _, e := range entries {
		size := 0
		if e.Type == "file" {
			if obj := repo.Git.GetObject(e.SHA); obj != nil {
				size = len(obj.Data)
			}
		}
		out = append(out, restContentEntryView{Type: e.Type, Name: e.Name, Path: e.Path, SHA: e.SHA, Size: size})
	}
	writeJSON(w, s, 200, out)
}

// restContentsCommit writes one commit on branch that sets path to data
// (or deletes it when data is nil) and moves the branch ref. It assumes
// the caller holds the store lock.
func restContentsCommit(repo *Repo, branch, tip, path string, data []byte, message string) (commitSHA string, ok bool) {
	files := restListFiles(repo.Git, tip)
	if data == nil {
		delete(files, path)
	} else {
		files[path] = repo.Git.WriteBlob(data)
	}
	treeSHA := restBuildTree(repo.Git, files)
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	commitSHA = repo.Git.WriteCommit(treeSHA, parents, message)
	if good, _ := repo.Git.UpdateRef("refs/heads/"+branch, commitSHA); !good {
		return "", false
	}
	return commitSHA, true
}

func (s *Store) restContentsPut(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, s, err)
		return
	}
	data, err := base64.StdEncoding.DecodeString(restCleanB64(body.Content))
	if err != nil {
		writeErr(w, s, Err(400, "Invalid base64 content"))
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip := repo.Git.ResolveRef("refs/heads/" + branch)
	if tip == "" {
		// Unborn branch: start from the default branch tip (implicit branch
		// creation, as GitHub does for a contents write on a new branch).
		// An unborn repository accepts the first commit on its default branch.
		if branch == repo.DefaultBranch {
			tip = ""
		} else {
			tip = repo.Git.ResolveRef("refs/heads/" + repo.DefaultBranch)
			if tip == "" {
				writeErr(w, s, Err(422, "Reference creation failed."))
				return
			}
		}
	}
	existing, found := repo.Git.GetFileAtCommit(tip, path)
	status := 201
	if found {
		if body.SHA == "" {
			writeErr(w, s, Err(422, "sha is required to update a file"))
			return
		}
		if body.SHA != sha1Hex("blob", existing) {
			writeErr(w, s, Err(409, "sha does not match the current file"))
			return
		}
		status = 200
	}
	commitSHA, ok := restContentsCommit(repo, branch, tip, path, data, body.Message)
	if !ok {
		writeErr(w, s, Err(422, "Commit failed."))
		return
	}
	writeJSON(w, s, status, struct {
		Content restContentFileView `json:"content"`
		Commit  restCommitRef       `json:"commit"`
	}{
		Content: restContentFileView{
			Type: "file", Encoding: "base64",
			Content: base64.StdEncoding.EncodeToString(data),
			SHA:     sha1Hex("blob", data), Size: len(data),
			Name: restBaseName(path), Path: path,
		},
		Commit: restCommitRef{SHA: commitSHA},
	})
}

func (s *Store) restContentsDelete(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	var body struct {
		Message string `json:"message"`
		Branch  string `json:"branch"`
		SHA     string `json:"sha"`
	}
	// A delete body may be absent; readJSON leaves dst zero on empty body.
	if err := readJSON(r, &body); err != nil {
		writeErr(w, s, err)
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip := repo.Git.ResolveRef("refs/heads/" + branch)
	if tip == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	if _, found := repo.Git.GetFileAtCommit(tip, path); !found {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	commitSHA, ok := restContentsCommit(repo, branch, tip, path, nil, body.Message)
	if !ok {
		writeErr(w, s, Err(422, "Commit failed."))
		return
	}
	writeJSON(w, s, 200, struct {
		Commit restCommitRef `json:"commit"`
	}{Commit: restCommitRef{SHA: commitSHA}})
}

// ---------------------------------------------------------------------------
// Labels
// ---------------------------------------------------------------------------

func (s *Store) handleLabels(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	switch r.Method {
	case http.MethodGet:
		names := make([]string, 0, len(repo.Labels))
		for name := range repo.Labels {
			names = append(names, name)
		}
		sort.Strings(names)
		out := make([]restLabelView, 0, len(names))
		for _, name := range names {
			out = append(out, restLabelViewOf(repo.Labels[name]))
		}
		writeJSON(w, s, 200, out)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Color       string `json:"color"`
			Description string `json:"description"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeErr(w, s, Err(422, "Label creation failed."))
			return
		}
		if _, exists := repo.Labels[body.Name]; exists {
			writeErr(w, s, Err(422, "Label already exists."))
			return
		}
		color := body.Color
		if color == "" {
			color = "ededed"
		}
		label := &Label{Name: body.Name, Color: color, Description: body.Description}
		repo.Labels[body.Name] = label
		writeJSON(w, s, 201, restLabelViewOf(label))
	default:
		methodNotAllowed(w, s)
	}
}

// ---------------------------------------------------------------------------
// Issues + comments
// ---------------------------------------------------------------------------

func (s *Store) handleIssues(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	switch r.Method {
	case http.MethodGet:
		perPage, page := restParsePageParams(r)
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		direction := r.URL.Query().Get("direction")
		var filtered []*Issue
		for _, iss := range repo.Issues {
			if state == "all" || iss.State == state {
				filtered = append(filtered, iss)
			}
		}
		if direction == "desc" {
			for i, j := 0, len(filtered)-1; i < j; i, j = i+1, j-1 {
				filtered[i], filtered[j] = filtered[j], filtered[i]
			}
		}
		start := (page - 1) * perPage
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + perPage
		if end > len(filtered) {
			end = len(filtered)
		}
		out := make([]restIssueView, 0, end-start)
		for _, iss := range filtered[start:end] {
			out = append(out, restIssueViewOf(repo, iss))
		}
		writeJSON(w, s, 200, out)
	case http.MethodPost:
		var body struct {
			Title  string   `json:"title"`
			Body   string   `json:"body"`
			Labels []string `json:"labels"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		for _, name := range body.Labels {
			if _, exists := repo.Labels[name]; !exists {
				writeErr(w, s, Err(422, "Label '"+name+"' does not exist"))
				return
			}
		}
		labels := body.Labels
		if labels == nil {
			labels = []string{}
		}
		iss := &Issue{
			Number: repo.nextIssueNum, Title: body.Title, Body: body.Body,
			State: "open", Labels: labels,
		}
		repo.nextIssueNum++
		repo.Issues = append(repo.Issues, iss)
		writeJSON(w, s, 201, restIssueViewOf(repo, iss))
	default:
		methodNotAllowed(w, s)
	}
}

// restIssueLocked looks an issue up by number. Numbers are sequential from
// 1 with no deletes, so index = number-1. Caller holds the store lock.
func restIssueLocked(repo *Repo, num int) *Issue {
	if num < 1 || num > len(repo.Issues) {
		return nil
	}
	return repo.Issues[num-1]
}

func (s *Store) handleIssue(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	num, good := restParseNum(r.PathValue("num"))
	if !good {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	iss := restIssueLocked(repo, num)
	if iss == nil {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s, 200, restIssueViewOf(repo, iss))
	case http.MethodPatch:
		var body struct {
			State *string `json:"state"`
			Title *string `json:"title"`
			Body  *string `json:"body"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		if body.State != nil && (*body.State == "open" || *body.State == "closed") {
			iss.State = *body.State
		}
		if body.Title != nil {
			iss.Title = *body.Title
		}
		if body.Body != nil {
			iss.Body = *body.Body
		}
		writeJSON(w, s, 200, restIssueViewOf(repo, iss))
	default:
		methodNotAllowed(w, s)
	}
}

func (s *Store) handleIssueComments(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	num, good := restParseNum(r.PathValue("num"))
	if !good {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	iss := restIssueLocked(repo, num)
	if iss == nil {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := make([]restCommentView, 0, len(iss.Comments))
		for _, c := range iss.Comments {
			out = append(out, restCommentView{ID: c.ID, Body: c.Body, CreatedAt: epochStr(), UpdatedAt: epochStr(), User: restViewerView()})
		}
		writeJSON(w, s, 200, out)
	case http.MethodPost:
		var body struct {
			Body string `json:"body"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		c := &Comment{ID: repo.nextCommentID, Body: body.Body}
		repo.nextCommentID++
		iss.Comments = append(iss.Comments, c)
		writeJSON(w, s, 201, restCommentView{ID: c.ID, Body: c.Body, CreatedAt: epochStr(), UpdatedAt: epochStr(), User: restViewerView()})
	default:
		methodNotAllowed(w, s)
	}
}

// ---------------------------------------------------------------------------
// Pull requests
// ---------------------------------------------------------------------------

// restPullMetaEntry remembers the shas a pull request was created with (and
// its merge sha once merged), so reads stay correct even if a branch ref
// moves or is deleted afterwards.
type restPullMetaEntry struct {
	headSHA  string
	baseSHA  string
	mergeSHA string
}

var restPullMeta = map[uint64]*restPullMetaEntry{}

func restPullKey(repoID, num int) uint64 { return uint64(repoID)<<32 | uint64(num) }

// restPullLocked looks a pull request up by number (sequential from 1).
func restPullLocked(repo *Repo, num int) *Pull {
	if num < 1 || num > len(repo.Pulls) {
		return nil
	}
	return repo.Pulls[num-1]
}

// createPullLocked creates an open pull request. Caller holds the store lock.
func createPullLocked(repo *Repo, title, body, head, base string) (*Pull, *APIError) {
	if base == "" {
		base = repo.DefaultBranch
	}
	headSHA := repo.Git.ResolveRef("refs/heads/" + head)
	if headSHA == "" {
		return nil, Err(422, "head branch not found")
	}
	baseSHA := repo.Git.ResolveRef("refs/heads/" + base)
	if baseSHA == "" {
		return nil, Err(422, "base branch not found")
	}
	pr := &Pull{
		Number: repo.nextPullNum, Title: title, Body: body,
		State: "open", Head: head, Base: base, Merged: false, Mergeable: true,
	}
	repo.nextPullNum++
	repo.Pulls = append(repo.Pulls, pr)
	restPullMeta[restPullKey(repo.ID, pr.Number)] = &restPullMetaEntry{headSHA: headSHA, baseSHA: baseSHA}
	return pr, nil
}

// createPullInRepo creates an open pull request; shared with the GraphQL
// worker. It takes the store lock itself — callers must NOT hold it.
func createPullInRepo(s *Store, repo *Repo, title, body, head, base string) (*Pull, *APIError) {
	s.restLock()
	defer s.restUnlock()
	return createPullLocked(repo, title, body, head, base)
}

func restPullViewOf(repo *Repo, pr *Pull) restPullView {
	headSHA := repo.Git.ResolveRef("refs/heads/" + pr.Head)
	baseSHA := repo.Git.ResolveRef("refs/heads/" + pr.Base)
	meta := restPullMeta[restPullKey(repo.ID, pr.Number)]
	if headSHA == "" && meta != nil {
		headSHA = meta.headSHA
	}
	if baseSHA == "" && meta != nil {
		baseSHA = meta.baseSHA
	}
	view := restPullView{
		ID: pr.Number, Number: pr.Number, Title: pr.Title, Body: pr.Body,
		State:  pr.State,
		Head:   restBranchRefView{Ref: pr.Head, SHA: headSHA, Label: repo.Owner + ":" + pr.Head},
		Base:   restBranchRefView{Ref: pr.Base, SHA: baseSHA, Label: repo.Owner + ":" + pr.Base},
		Merged: pr.Merged, Mergeable: pr.Mergeable,
		CreatedAt: epochStr(), UpdatedAt: epochStr(), User: restViewerView(),
	}
	if pr.Merged {
		view.MergedAt = epochStr()
		if meta != nil && meta.mergeSHA != "" {
			view.MergeCommitSHA = meta.mergeSHA
		}
	}
	return view
}

func (s *Store) handlePulls(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	switch r.Method {
	case http.MethodGet:
		perPage, page := restParsePageParams(r)
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		var filtered []*Pull
		for _, pr := range repo.Pulls {
			if state == "all" || pr.State == state {
				filtered = append(filtered, pr)
			}
		}
		start := (page - 1) * perPage
		if start > len(filtered) {
			start = len(filtered)
		}
		end := start + perPage
		if end > len(filtered) {
			end = len(filtered)
		}
		out := make([]restPullView, 0, end-start)
		for _, pr := range filtered[start:end] {
			out = append(out, restPullViewOf(repo, pr))
		}
		writeJSON(w, s, 200, out)
	case http.MethodPost:
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, s, err)
			return
		}
		pr, apiErr := createPullLocked(repo, body.Title, body.Body, body.Head, body.Base)
		if apiErr != nil {
			writeErr(w, s, apiErr)
			return
		}
		writeJSON(w, s, 201, restPullViewOf(repo, pr))
	default:
		methodNotAllowed(w, s)
	}
}

func (s *Store) handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	num, good := restParseNum(r.PathValue("num"))
	if !good {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	pr := restPullLocked(repo, num)
	if pr == nil {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	writeJSON(w, s, 200, restPullViewOf(repo, pr))
}

func (s *Store) handlePullFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	num, good := restParseNum(r.PathValue("num"))
	if !good {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	pr := restPullLocked(repo, num)
	if pr == nil {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	headSHA := repo.Git.ResolveRef("refs/heads/" + pr.Head)
	baseSHA := repo.Git.ResolveRef("refs/heads/" + pr.Base)
	if headSHA == "" || baseSHA == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	mb := restMergeBase(repo.Git, headSHA, baseSHA)
	changes := restDiffFiles(repo.Git, mb, headSHA)
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	out := make([]restPullFileView, 0, len(changes))
	for _, ch := range changes {
		var oldData, newData []byte
		if ch.FromSHA != "" {
			if obj := repo.Git.GetObject(ch.FromSHA); obj != nil {
				oldData = obj.Data
			}
		}
		if ch.ToSHA != "" {
			if obj := repo.Git.GetObject(ch.ToSHA); obj != nil {
				newData = obj.Data
			}
		}
		add, del := restLineDiff(oldData, newData)
		status := "modified"
		if ch.FromSHA == "" {
			status = "added"
		} else if ch.ToSHA == "" {
			status = "removed"
		}
		out = append(out, restPullFileView{
			Filename: ch.Path, Status: status,
			Additions: add, Deletions: del, Changes: add + del,
		})
	}
	writeJSON(w, s, 200, out)
}

func (s *Store) handlePullMerge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, s)
		return
	}
	repo, ok := s.restRepoLocked(w, r)
	if !ok {
		return
	}
	defer s.restUnlock()
	num, good := restParseNum(r.PathValue("num"))
	if !good {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	pr := restPullLocked(repo, num)
	if pr == nil {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	// Consume the body (merge_method etc.); a malformed body is a 400.
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, s, err)
		return
	}
	if pr.Merged || pr.State != "open" {
		writeErr(w, s, Err(405, "Pull request is already merged"))
		return
	}
	headSHA := repo.Git.ResolveRef("refs/heads/" + pr.Head)
	baseSHA := repo.Git.ResolveRef("refs/heads/" + pr.Base)
	if headSHA == "" || baseSHA == "" {
		writeErr(w, s, Err(404, "Not Found"))
		return
	}
	g := repo.Git
	mb := restMergeBase(g, headSHA, baseSHA)
	changes := restDiffFiles(g, mb, headSHA)
	baseFiles := restListFiles(g, baseSHA)
	mbFiles := restListFiles(g, mb)
	for _, ch := range changes {
		// Conflict: the file also changed on the base side since the
		// merge-base, and the two sides disagree on the result.
		if baseFiles[ch.Path] != mbFiles[ch.Path] && ch.ToSHA != baseFiles[ch.Path] {
			pr.Mergeable = false
			writeErr(w, s, Err(405, "Merge conflict"))
			return
		}
	}
	// Squash: apply the head's changes onto the base tip.
	newFiles := make(map[string]string, len(baseFiles))
	for path, sha := range baseFiles {
		newFiles[path] = sha
	}
	for _, ch := range changes {
		if ch.ToSHA == "" {
			delete(newFiles, ch.Path)
		} else {
			newFiles[ch.Path] = ch.ToSHA
		}
	}
	treeSHA := restBuildTree(g, newFiles)
	message := "Merge pull request #" + itoa(pr.Number) + " from " + repo.Owner + "/" + pr.Head
	mergeSHA := g.WriteCommit(treeSHA, []string{baseSHA}, message)
	if good, _ := g.UpdateRef("refs/heads/"+pr.Base, mergeSHA); !good {
		writeErr(w, s, Err(422, "Merge failed."))
		return
	}
	pr.State = "closed"
	pr.Merged = true
	pr.Mergeable = true
	if meta := restPullMeta[restPullKey(repo.ID, pr.Number)]; meta != nil {
		meta.mergeSHA = mergeSHA
	}
	// The base moved: recompute mergeability of the other open PRs.
	for _, other := range repo.Pulls {
		if other == pr || other.State != "open" || other.Base != pr.Base {
			continue
		}
		other.Mergeable = !restPullConflicts(g, other)
	}
	writeJSON(w, s, 200, struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}{SHA: mergeSHA, Merged: true, Message: message})
}

// restPullConflicts reports whether merging pr now would hit a conflict.
func restPullConflicts(g *GitRepo, pr *Pull) bool {
	headSHA := g.ResolveRef("refs/heads/" + pr.Head)
	baseSHA := g.ResolveRef("refs/heads/" + pr.Base)
	if headSHA == "" || baseSHA == "" {
		return false
	}
	mb := restMergeBase(g, headSHA, baseSHA)
	changes := restDiffFiles(g, mb, headSHA)
	baseFiles := restListFiles(g, baseSHA)
	mbFiles := restListFiles(g, mb)
	for _, ch := range changes {
		if baseFiles[ch.Path] != mbFiles[ch.Path] && ch.ToSHA != baseFiles[ch.Path] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Git object walking (standard git object formats; uses GetObject only).
// ---------------------------------------------------------------------------

// restParseCommit parses a git commit object: tree sha, parent shas, the
// message after the blank line, and the author name/email.
func restParseCommit(data []byte) (tree string, parents []string, message string, authorName, authorEmail string) {
	text := string(data)
	head, msg := text, ""
	if idx := strings.Index(text, "\n\n"); idx >= 0 {
		head, msg = text[:idx], text[idx+2:]
	}
	message = msg
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			authorName, authorEmail = restParseAuthor(line[7:])
		}
	}
	return tree, parents, message, authorName, authorEmail
}

func restParseAuthor(v string) (string, string) {
	lt := strings.Index(v, "<")
	gt := strings.Index(v, ">")
	if lt >= 0 && gt > lt {
		return strings.TrimSpace(v[:lt]), v[lt+1 : gt]
	}
	return strings.TrimSpace(v), ""
}

// restMergeBase finds the first common ancestor of a and b (BFS from a over
// the ancestor set of b). "" when unrelated or unknown.
func restMergeBase(g *GitRepo, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	if a == b {
		return a
	}
	anc := make(map[string]bool)
	stack := []string{b}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if anc[cur] {
			continue
		}
		anc[cur] = true
		obj := g.GetObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents, _, _, _ := restParseCommit(obj.Data)
		stack = append(stack, parents...)
	}
	seen := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if anc[cur] {
			return cur
		}
		obj := g.GetObject(cur)
		if obj == nil || obj.Type != "commit" {
			continue
		}
		_, parents, _, _, _ := restParseCommit(obj.Data)
		queue = append(queue, parents...)
	}
	return ""
}

// restFileChange is one path that differs between two commits.
type restFileChange struct {
	Path    string
	FromSHA string // "" when the path did not exist
	ToSHA   string // "" when the path was deleted
}

// restDiffFiles lists every path whose blob differs between two commits.
func restDiffFiles(g *GitRepo, fromCommit, toCommit string) []restFileChange {
	from := restListFiles(g, fromCommit)
	to := restListFiles(g, toCommit)
	var out []restFileChange
	for path, fromSHA := range from {
		if toSHA, ok := to[path]; !ok {
			out = append(out, restFileChange{Path: path, FromSHA: fromSHA})
		} else if toSHA != fromSHA {
			out = append(out, restFileChange{Path: path, FromSHA: fromSHA, ToSHA: toSHA})
		}
	}
	for path, toSHA := range to {
		if _, ok := from[path]; !ok {
			out = append(out, restFileChange{Path: path, ToSHA: toSHA})
		}
	}
	return out
}

// restListFiles returns path -> blob sha for every file in a commit's tree.
func restListFiles(g *GitRepo, commitSHA string) map[string]string {
	out := make(map[string]string)
	if commitSHA == "" {
		return out
	}
	var walk func(dir string)
	walk = func(dir string) {
		for _, e := range g.ListDirAtCommit(commitSHA, dir) {
			if e.Type == "file" {
				out[e.Path] = e.SHA
			} else if e.Type == "dir" {
				walk(e.Path)
			}
		}
	}
	walk("")
	return out
}

func restTreeSortKey(e TreeEntry) string {
	if e.Mode == "40000" {
		return e.Name + "/"
	}
	return e.Name
}

// restBuildTree writes a nested tree for files (path -> blob sha) and
// returns the root tree sha.
func restBuildTree(g *GitRepo, files map[string]string) string {
	return restWriteDir(g, files, "")
}

func restWriteDir(g *GitRepo, files map[string]string, dir string) string {
	entries := []TreeEntry{}
	seenDirs := make(map[string]bool)
	for path, sha := range files {
		rel := path
		if dir != "" {
			if !strings.HasPrefix(path, dir+"/") {
				continue
			}
			rel = path[len(dir)+1:]
		}
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) == 1 {
			entries = append(entries, TreeEntry{Mode: "100644", Name: parts[0], SHA: sha})
		} else if !seenDirs[parts[0]] {
			seenDirs[parts[0]] = true
			sub := dir
			if sub == "" {
				sub = parts[0]
			} else {
				sub = sub + "/" + parts[0]
			}
			entries = append(entries, TreeEntry{Mode: "40000", Name: parts[0], SHA: restWriteDir(g, files, sub)})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return restTreeSortKey(entries[i]) < restTreeSortKey(entries[j]) })
	return g.WriteTree(entries)
}

// restLineDiff counts added / removed lines between two file versions
// (LCS over lines).
func restLineDiff(oldData, newData []byte) (add, del int) {
	oldLines := restSplitLines(oldData)
	newLines := restSplitLines(newData)
	m, n := len(oldLines), len(newLines)
	if m == 0 {
		return n, 0
	}
	if n == 0 {
		return 0, m
	}
	prev := make([]int, n+1)
	cur := make([]int, n+1)
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if oldLines[i-1] == newLines[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] > cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	lcs := prev[n]
	return n - lcs, m - lcs
}

func restSplitLines(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// mergePullSquash squashes PR p onto its base branch and advances the base
// ref. Called WITH s.mu held (both GraphQL mergePullRequest and the MCP
// merge_pull_request tool lock before calling). Mirrors the core of
// handlePullMerge (same helpers, same conflict rule, same message format);
// keep the two in sync. Returns the merge commit SHA.
func mergePullSquash(repo *Repo, p *Pull) (string, *APIError) {
	if repo == nil || p == nil || repo.Git == nil {
		return "", Err(404, "Not Found")
	}
	if p.Merged || p.State != "open" {
		return "", Err(405, "Pull request is already merged")
	}
	headSHA := repo.Git.ResolveRef("refs/heads/" + p.Head)
	baseSHA := repo.Git.ResolveRef("refs/heads/" + p.Base)
	if headSHA == "" || baseSHA == "" {
		return "", Err(404, "Not Found")
	}
	g := repo.Git
	mb := restMergeBase(g, headSHA, baseSHA)
	changes := restDiffFiles(g, mb, headSHA)
	baseFiles := restListFiles(g, baseSHA)
	mbFiles := restListFiles(g, mb)
	for _, ch := range changes {
		if baseFiles[ch.Path] != mbFiles[ch.Path] && ch.ToSHA != baseFiles[ch.Path] {
			p.Mergeable = false
			return "", Err(405, "Merge conflict")
		}
	}
	newFiles := make(map[string]string, len(baseFiles))
	for path, sha := range baseFiles {
		newFiles[path] = sha
	}
	for _, ch := range changes {
		if ch.ToSHA == "" {
			delete(newFiles, ch.Path)
		} else {
			newFiles[ch.Path] = ch.ToSHA
		}
	}
	treeSHA := restBuildTree(g, newFiles)
	message := "Merge pull request #" + itoa(p.Number) + " from " + repo.Owner + "/" + p.Head
	mergeSHA := g.WriteCommit(treeSHA, []string{baseSHA}, message)
	if good, _ := g.UpdateRef("refs/heads/"+p.Base, mergeSHA); !good {
		return "", Err(422, "Merge failed.")
	}
	p.State = "closed"
	p.Merged = true
	p.Mergeable = true
	if meta := restPullMeta[restPullKey(repo.ID, p.Number)]; meta != nil {
		meta.mergeSHA = mergeSHA
	}
	for _, other := range repo.Pulls {
		if other == p || other.State != "open" || other.Base != p.Base {
			continue
		}
		other.Mergeable = !restPullConflicts(g, other)
	}
	return mergeSHA, nil
}
