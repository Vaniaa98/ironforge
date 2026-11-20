package main

// GraphQL v4 endpoint (POST /api/graphql) — worker 3.
//
// Scope: exactly the documents gh and github-mcp-server send, as defined by
// program/env/src/checks-graphql.ts + graphql-client.ts. No general GraphQL
// library (stdlib only): a small tokenizer + recursive-descent parser for
// the fixed-shape documents, plus a hand-written executor over the Store.
//
// Determinism: no wall clock, no randomness; every timestamp is epochStr();
// list order is by issue/pull number; map keys are sorted by encoding/json.

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Global (Relay) ids: base64("Kind:part:part..."). Opaque: never the plain
// database id or number, stable across reads.
// ---------------------------------------------------------------------------

func gqlGlobalID(kind string, parts ...string) string {
	raw := kind
	for _, p := range parts {
		raw += ":" + p
	}
	return base64.StdEncoding.EncodeToString([]byte(raw))
}

func gqlDecodeGlobalID(id string) (kind string, parts []string, ok bool) {
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		b, err = base64.URLEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	segs := strings.Split(string(b), ":")
	if len(segs) < 2 {
		return "", nil, false
	}
	return segs[0], segs[1:], true
}

// gqlCursor is the opaque cursor for issues pagination: base64("cursor:<n>").
func gqlCursor(number int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + itoa(number)))
}

func gqlCursorNumber(c string) (int, bool) {
	b, err := base64.StdEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	s := string(b)
	if !strings.HasPrefix(s, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(s[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Store helpers used by GraphQL mutations (each locks internally; callers
// must NOT hold s.mu when calling these).
// ---------------------------------------------------------------------------

func gqlRepoByID(s *Store, id int) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// gqlAddIssueToRepo appends an open issue to repo (locks internally).
func gqlAddIssueToRepo(s *Store, repo *Repo, title, body string) *Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	iss := &Issue{Number: repo.nextIssueNum, Title: title, Body: body, State: "open"}
	repo.nextIssueNum++
	repo.Issues = append(repo.Issues, iss)
	return iss
}

func gqlCloseIssueByID(s *Store, repoID, number int) (*Issue, *APIError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var repo *Repo
	for _, r := range s.repos {
		if r.ID == repoID {
			repo = r
			break
		}
	}
	if repo == nil {
		return nil, Err(404, "Not Found")
	}
	for _, iss := range repo.Issues {
		if iss.Number == number {
			iss.State = "closed"
			return iss, nil
		}
	}
	return nil, Err(404, "Not Found")
}

func gqlAddCommentByID(s *Store, repoID, number int, body string) (*Repo, *Issue, *Comment, *APIError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var repo *Repo
	for _, r := range s.repos {
		if r.ID == repoID {
			repo = r
			break
		}
	}
	if repo == nil {
		return nil, nil, nil, Err(404, "Not Found")
	}
	for _, iss := range repo.Issues {
		if iss.Number == number {
			c := &Comment{ID: repo.nextCommentID, Body: body}
			repo.nextCommentID++
			iss.Comments = append(iss.Comments, c)
			return repo, iss, c, nil
		}
	}
	return nil, nil, nil, Err(404, "Not Found")
}

// ---------------------------------------------------------------------------
// Git helpers: changed files of a pull request = paths whose blob differs
// between the head tree and the base tree (or is only in the head tree).
// ---------------------------------------------------------------------------

// gqlCommitTree returns the tree sha of a commit object, or "".
func gqlCommitTree(g *GitRepo, commitSHA string) string {
	if g == nil {
		return ""
	}
	obj := g.GetObject(commitSHA)
	if obj == nil || obj.Type != "commit" {
		return ""
	}
	line := string(obj.Data)
	if i := strings.Index(line, "\n"); i >= 0 {
		line = line[:i]
	}
	if strings.HasPrefix(line, "tree ") {
		return strings.TrimSpace(line[5:])
	}
	return ""
}

// gqlTreeEntries fills out[path] = blob sha for every blob under treeSHA.
func gqlTreeEntries(g *GitRepo, treeSHA, prefix string, out map[string]string) {
	obj := g.GetObject(treeSHA)
	if obj == nil || obj.Type != "tree" {
		return
	}
	data := obj.Data
	i := 0
	for i < len(data) {
		j := i
		for j < len(data) && data[j] != ' ' {
			j++
		}
		if j >= len(data) {
			return
		}
		mode := string(data[i:j])
		k := j + 1
		for k < len(data) && data[k] != 0 {
			k++
		}
		if k >= len(data) || k+21 > len(data) {
			return
		}
		name := string(data[j+1 : k])
		sha := hex.EncodeToString(data[k+1 : k+21])
		if mode == "40000" || mode == "040000" {
			gqlTreeEntries(g, sha, prefix+name+"/", out)
		} else {
			out[prefix+name] = sha
		}
		i = k + 21
	}
}

func gqlPullFiles(repo *Repo, p *Pull) []string {
	files := []string{}
	if repo == nil || repo.Git == nil {
		return files
	}
	headTree := gqlCommitTree(repo.Git, repo.Git.Refs["refs/heads/"+p.Head])
	baseTree := gqlCommitTree(repo.Git, repo.Git.Refs["refs/heads/"+p.Base])
	head := map[string]string{}
	base := map[string]string{}
	if headTree != "" {
		gqlTreeEntries(repo.Git, headTree, "", head)
	}
	if baseTree != "" {
		gqlTreeEntries(repo.Git, baseTree, "", base)
	}
	for path, sha := range head {
		if bsha, ok := base[path]; !ok || bsha != sha {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files
}

// ---------------------------------------------------------------------------
// Lexer for the fixed-shape GraphQL documents.
// ---------------------------------------------------------------------------

type gqlTokKind int

const (
	gqlEOF gqlTokKind = iota
	gqlLBrace
	gqlRBrace
	gqlLParen
	gqlRParen
	gqlLBracket
	gqlRBracket
	gqlColon
	gqlDollar
	gqlBang
	gqlEq
	gqlAt
	gqlSpread
	gqlName
	gqlString
	gqlInt
	gqlFloat
)

type gqlTok struct {
	kind gqlTokKind
	text string
}

func gqlLex(src string) ([]gqlTok, error) {
	var toks []gqlTok
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '{':
			toks = append(toks, gqlTok{gqlLBrace, "{"})
			i++
		case c == '}':
			toks = append(toks, gqlTok{gqlRBrace, "}"})
			i++
		case c == '(':
			toks = append(toks, gqlTok{gqlLParen, "("})
			i++
		case c == ')':
			toks = append(toks, gqlTok{gqlRParen, ")"})
			i++
		case c == '[':
			toks = append(toks, gqlTok{gqlLBracket, "["})
			i++
		case c == ']':
			toks = append(toks, gqlTok{gqlRBracket, "]"})
			i++
		case c == ':':
			toks = append(toks, gqlTok{gqlColon, ":"})
			i++
		case c == '$':
			toks = append(toks, gqlTok{gqlDollar, "$"})
			i++
		case c == '!':
			toks = append(toks, gqlTok{gqlBang, "!"})
			i++
		case c == '=':
			toks = append(toks, gqlTok{gqlEq, "="})
			i++
		case c == '@':
			toks = append(toks, gqlTok{gqlAt, "@"})
			i++
		case c == '.':
			if i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.' {
				toks = append(toks, gqlTok{gqlSpread, "..."})
				i += 3
			} else {
				return nil, fmt.Errorf("unexpected character '.'")
			}
		case c == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
					switch src[j] {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					case '"':
						sb.WriteByte('"')
					case '\\':
						sb.WriteByte('\\')
					case '/':
						sb.WriteByte('/')
					case 'u':
						if j+4 < len(src) {
							if code, err := strconv.ParseInt(src[j+1:j+5], 16, 32); err == nil {
								sb.WriteRune(rune(code))
							}
							j += 4
						}
					default:
						sb.WriteByte(src[j])
					}
					j++
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			if j >= len(src) {
				return nil, fmt.Errorf("unterminated string")
			}
			toks = append(toks, gqlTok{gqlString, sb.String()})
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i
			if src[j] == '-' {
				j++
			}
			isFloat := false
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if isFloat {
				toks = append(toks, gqlTok{gqlFloat, src[i:j]})
			} else {
				toks = append(toks, gqlTok{gqlInt, src[i:j]})
			}
			i = j
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_':
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			toks = append(toks, gqlTok{gqlName, src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	toks = append(toks, gqlTok{gqlEOF, ""})
	return toks, nil
}

// ---------------------------------------------------------------------------
// Recursive-descent parser (documents only; no schema validation here).
// ---------------------------------------------------------------------------

type gqlValKind int

const (
	gqlNull gqlValKind = iota
	gqlStr
	gqlIntVal
	gqlFloatVal
	gqlBoolVal
	gqlEnumVal
	gqlVarVal
	gqlListVal
	gqlObjVal
)

type gqlVal struct {
	kind gqlValKind
	s    string
	n    int64
	f    float64
	b    bool
	list []gqlVal
	obj  []gqlArg
}

type gqlArg struct {
	name string
	val  gqlVal
}

type gqlField struct {
	alias    string
	name     string
	args     []gqlArg
	children []*gqlField
	fragOn   string // non-empty for `... on Type`
}

func gqlFieldKey(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

type gqlOp struct {
	opType string // "query" | "mutation"
	name   string
	fields []*gqlField
}

type gqlParser struct {
	toks []gqlTok
	pos  int
}

func (p *gqlParser) peek() gqlTok  { return p.toks[p.pos] }
func (p *gqlParser) next() gqlTok  { t := p.toks[p.pos]; p.pos++; return t }
func (p *gqlParser) atEnd() bool   { return p.peek().kind == gqlEOF }
func (p *gqlParser) expect(k gqlTokKind) (gqlTok, error) {
	t := p.next()
	if t.kind != k {
		return t, fmt.Errorf("expected token %d, got %q", k, t.text)
	}
	return t, nil
}

// skipBalanced skips a parenthesised group (variable definitions, directive
// arguments); tokens are atomic so strings cannot confuse it.
func (p *gqlParser) skipBalanced(open, close gqlTokKind) error {
	if _, err := p.expect(open); err != nil {
		return err
	}
	depth := 1
	for depth > 0 {
		t := p.next()
		if t.kind == gqlEOF {
			return fmt.Errorf("unterminated group")
		}
		if t.kind == open {
			depth++
		} else if t.kind == close {
			depth--
		}
	}
	return nil
}

func (p *gqlParser) skipDirectives() error {
	for p.peek().kind == gqlAt {
		p.next()
		if _, err := p.expect(gqlName); err != nil {
			return err
		}
		if p.peek().kind == gqlLParen {
			if err := p.skipBalanced(gqlLParen, gqlRParen); err != nil {
				return err
			}
		}
	}
	return nil
}

func gqlParseDocument(src string) ([]*gqlOp, error) {
	toks, err := gqlLex(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	var ops []*gqlOp
	for !p.atEnd() {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (*gqlOp, error) {
	op := &gqlOp{opType: "query"}
	t := p.peek()
	if t.kind == gqlLBrace {
		fields, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		op.fields = fields
		return op, nil
	}
	if t.kind == gqlName && (t.text == "query" || t.text == "mutation" || t.text == "subscription") {
		op.opType = p.next().text
		if p.peek().kind == gqlName {
			op.name = p.next().text
		}
	} else if t.kind == gqlName {
		op.name = p.next().text
	} else {
		return nil, fmt.Errorf("expected an operation, got %q", t.text)
	}
	if p.peek().kind == gqlLParen {
		if err := p.skipBalanced(gqlLParen, gqlRParen); err != nil {
			return nil, err
		}
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	fields, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	op.fields = fields
	return op, nil
}

func (p *gqlParser) parseSelectionSet() ([]*gqlField, error) {
	if _, err := p.expect(gqlLBrace); err != nil {
		return nil, err
	}
	var fields []*gqlField
	for p.peek().kind != gqlRBrace {
		if p.peek().kind == gqlEOF {
			return nil, fmt.Errorf("unterminated selection set")
		}
		f, err := p.parseField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
	}
	p.next() // }
	return fields, nil
}

func (p *gqlParser) parseField() (*gqlField, error) {
	if p.peek().kind == gqlSpread {
		p.next()
		f := &gqlField{}
		if p.peek().kind == gqlName && p.peek().text == "on" {
			p.next()
			nameTok, err := p.expect(gqlName)
			if err != nil {
				return nil, err
			}
			f.fragOn = nameTok.text
		} else {
			return nil, fmt.Errorf("named fragment spreads are not supported")
		}
		if err := p.skipDirectives(); err != nil {
			return nil, err
		}
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
		return f, nil
	}
	nameTok, err := p.expect(gqlName)
	if err != nil {
		return nil, err
	}
	f := &gqlField{name: nameTok.text}
	if p.peek().kind == gqlColon {
		p.next()
		aliasTok, err := p.expect(gqlName)
		if err != nil {
			return nil, err
		}
		f.alias = f.name
		f.name = aliasTok.text
	}
	if p.peek().kind == gqlLParen {
		args, err := p.parseArgs()
		if err != nil {
			return nil, err
		}
		f.args = args
	}
	if err := p.skipDirectives(); err != nil {
		return nil, err
	}
	if p.peek().kind == gqlLBrace {
		children, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		f.children = children
	}
	return f, nil
}

func (p *gqlParser) parseArgs() ([]gqlArg, error) {
	if _, err := p.expect(gqlLParen); err != nil {
		return nil, err
	}
	var args []gqlArg
	for p.peek().kind != gqlRParen {
		if p.peek().kind == gqlEOF {
			return nil, fmt.Errorf("unterminated argument list")
		}
		nameTok, err := p.expect(gqlName)
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(gqlColon); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args = append(args, gqlArg{name: nameTok.text, val: v})
	}
	p.next() // )
	return args, nil
}

func (p *gqlParser) parseValue() (gqlVal, error) {
	t := p.next()
	switch t.kind {
	case gqlDollar:
		nameTok, err := p.expect(gqlName)
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: gqlVarVal, s: nameTok.text}, nil
	case gqlString:
		return gqlVal{kind: gqlStr, s: t.text}, nil
	case gqlInt:
		n, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: gqlIntVal, n: n}, nil
	case gqlFloat:
		f, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return gqlVal{}, err
		}
		return gqlVal{kind: gqlFloatVal, f: f}, nil
	case gqlLBracket:
		var list []gqlVal
		for p.peek().kind != gqlRBracket {
			if p.peek().kind == gqlEOF {
				return gqlVal{}, fmt.Errorf("unterminated list")
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			list = append(list, v)
		}
		p.next()
		return gqlVal{kind: gqlListVal, list: list}, nil
	case gqlLBrace:
		var obj []gqlArg
		for p.peek().kind != gqlRBrace {
			if p.peek().kind == gqlEOF {
				return gqlVal{}, fmt.Errorf("unterminated input object")
			}
			nameTok, err := p.expect(gqlName)
			if err != nil {
				return gqlVal{}, err
			}
			if _, err := p.expect(gqlColon); err != nil {
				return gqlVal{}, err
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlVal{}, err
			}
			obj = append(obj, gqlArg{name: nameTok.text, val: v})
		}
		p.next()
		return gqlVal{kind: gqlObjVal, obj: obj}, nil
	case gqlName:
		switch t.text {
		case "true":
			return gqlVal{kind: gqlBoolVal, b: true}, nil
		case "false":
			return gqlVal{kind: gqlBoolVal, b: false}, nil
		case "null":
			return gqlVal{kind: gqlNull}, nil
		default:
			return gqlVal{kind: gqlEnumVal, s: t.text}, nil
		}
	}
	return gqlVal{}, fmt.Errorf("unexpected value token %q", t.text)
}

// ---------------------------------------------------------------------------
// Argument resolution against the operation variables.
// ---------------------------------------------------------------------------

func gqlResolveValue(v gqlVal, vars map[string]any) any {
	switch v.kind {
	case gqlVarVal:
		return vars[v.s]
	case gqlListVal:
		arr := []any{}
		for _, e := range v.list {
			arr = append(arr, gqlResolveValue(e, vars))
		}
		return arr
	case gqlObjVal:
		m := map[string]any{}
		for _, e := range v.obj {
			m[e.name] = gqlResolveValue(e.val, vars)
		}
		return m
	case gqlStr, gqlEnumVal:
		return v.s
	case gqlIntVal:
		return v.n
	case gqlFloatVal:
		return v.f
	case gqlBoolVal:
		return v.b
	default:
		return nil
	}
}

func gqlResolveArgs(f *gqlField, vars map[string]any) map[string]any {
	out := map[string]any{}
	for _, a := range f.args {
		out[a.name] = gqlResolveValue(a.val, vars)
	}
	return out
}

func gqlArgStr(m map[string]any, name string) string {
	if v, ok := m[name]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func gqlArgInt(m map[string]any, name string, def int) int {
	v, ok := m[name]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case int64:
		return int(n)
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	}
	return def
}

func gqlArgStrList(m map[string]any, name string) []string {
	v, ok := m[name]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Introspection tables (static schema surface for gh's feature probes).
// ---------------------------------------------------------------------------

var gqlTypeFields = map[string][]string{
	"Issue":                         {"id", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "author", "labels", "comments", "closed"},
	"PullRequest":                   {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "url", "createdAt", "updatedAt", "author"},
	"Repository":                    {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "private", "url", "createdAt", "updatedAt"},
	"User":                          {"login", "id", "name", "email"},
	"IssueConnection":               {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestConnection":         {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFileConnection": {"nodes", "edges", "pageInfo", "totalCount"},
	"PullRequestChangedFile":        {"path", "additions", "deletions"},
	"IssueComment":                  {"id", "body", "author", "createdAt", "updatedAt"},
	"Ref":                           {"name", "prefix", "id"},
	"PageInfo":                      {"hasNextPage", "hasPreviousPage", "endCursor", "startCursor"},
}

var gqlTypeEnums = map[string][]string{
	"SearchType":           {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "CODE", "TOPIC", "PACKAGE", "MILESTONE", "WIKI"},
	"IssueState":           {"OPEN", "CLOSED"},
	"OrderDirection":       {"ASC", "DESC"},
	"IssueOrderField":      {"CREATED_AT", "UPDATED_AT", "COMMENTS"},
	"MergeableState":       {"MERGEABLE", "CONFLICTING", "UNKNOWN"},
	"PullRequestState":     {"OPEN", "CLOSED", "MERGED"},
	"PullRequestMergeMethod": {"MERGE", "SQUASH", "REBASE"},
	"RepositoryPermission": {"ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"},
}

// ---------------------------------------------------------------------------
// Executor.
// ---------------------------------------------------------------------------

type gqlExec struct {
	s    *Store
	vars map[string]any
	errs []map[string]any
}

func (e *gqlExec) errf(path []any, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	entry := map[string]any{"message": msg}
	if len(path) > 0 {
		entry["path"] = path
	}
	e.errs = append(e.errs, entry)
}

func gqlIssueState(iss *Issue) string {
	if iss.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func gqlPullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func gqlMergeable(p *Pull) string {
	if p.Mergeable {
		return "MERGEABLE"
	}
	return "UNKNOWN"
}

// resolveUser resolves a User/Viewer selection set.
func (e *gqlExec) resolveUser(u *User, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" {
			if f.fragOn == "User" || f.fragOn == "Node" {
				for k, v := range e.resolveUser(u, f.children, path) {
					out[k] = v
				}
			}
			continue
		}
		key := gqlFieldKey(f)
		p := append(append([]any{}, path...), key)
		switch f.name {
		case "login":
			out[key] = u.Login
		case "id":
			out[key] = gqlGlobalID("User", itoa(u.ID))
		case "name":
			out[key] = u.Name
		case "email":
			out[key] = u.Email
		default:
			e.errf(p, "Cannot query field %q on type %q", f.name, "User")
		}
	}
	return out
}

// resolveIssue resolves an Issue selection set.
func (e *gqlExec) resolveIssue(repo *Repo, iss *Issue, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" {
			if f.fragOn == "Issue" || f.fragOn == "Node" {
				for k, v := range e.resolveIssue(repo, iss, f.children, path) {
					out[k] = v
				}
			}
			continue
		}
		key := gqlFieldKey(f)
		p := append(append([]any{}, path...), key)
		switch f.name {
		case "id":
			out[key] = gqlGlobalID("Issue", itoa(repo.ID), itoa(iss.Number))
		case "number":
			out[key] = iss.Number
		case "title":
			out[key] = iss.Title
		case "body":
			out[key] = iss.Body
		case "state":
			out[key] = gqlIssueState(iss)
		case "url":
			out[key] = "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(iss.Number)
		case "createdAt", "updatedAt":
			out[key] = epochStr()
		case "closed":
			out[key] = iss.State == "closed"
		case "author":
			out[key] = e.resolveUser(viewerUser(), f.children, p)
		case "labels":
			nodes := []any{}
			for _, name := range iss.Labels {
				nodes = append(nodes, map[string]any{"name": name})
			}
			out[key] = map[string]any{"nodes": nodes, "totalCount": len(nodes)}
		case "comments":
			nodes := []any{}
			var nodeFields []*gqlField
			for _, cf := range f.children {
				if cf.name == "nodes" {
					nodeFields = cf.children
				}
			}
			for _, c := range iss.Comments {
				nodes = append(nodes, e.resolveComment(repo, iss, c, nodeFields, p))
			}
			out[key] = map[string]any{"nodes": nodes, "totalCount": len(nodes)}
		default:
			e.errf(p, "Cannot query field %q on type %q", f.name, "Issue")
		}
	}
	return out
}

// resolveComment resolves an IssueComment node (used by addComment).
func (e *gqlExec) resolveComment(repo *Repo, iss *Issue, c *Comment, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" {
			if f.fragOn == "IssueComment" || f.fragOn == "Node" {
				for k, v := range e.resolveComment(repo, iss, c, f.children, path) {
					out[k] = v
				}
			}
			continue
		}
		key := gqlFieldKey(f)
		p := append(append([]any{}, path...), key)
		switch f.name {
		case "id":
			out[key] = gqlGlobalID("Comment", itoa(repo.ID), itoa(iss.Number), itoa(c.ID))
		case "body":
			out[key] = c.Body
		case "createdAt", "updatedAt":
			out[key] = epochStr()
		case "author":
			out[key] = e.resolveUser(viewerUser(), f.children, p)
		default:
			e.errf(p, "Cannot query field %q on type %q", f.name, "IssueComment")
		}
	}
	return out
}

// resolvePull resolves a PullRequest selection set.
func (e *gqlExec) resolvePull(repo *Repo, p *Pull, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" {
			if f.fragOn == "PullRequest" || f.fragOn == "Node" {
				for k, v := range e.resolvePull(repo, p, f.children, path) {
					out[k] = v
				}
			}
			continue
		}
		key := gqlFieldKey(f)
		pth := append(append([]any{}, path...), key)
		switch f.name {
		case "id":
			out[key] = gqlGlobalID("PullRequest", itoa(repo.ID), itoa(p.Number))
		case "number":
			out[key] = p.Number
		case "title":
			out[key] = p.Title
		case "body":
			out[key] = p.Body
		case "state":
			out[key] = gqlPullState(p)
		case "merged":
			out[key] = p.Merged
		case "mergeable":
			out[key] = gqlMergeable(p)
		case "headRefName":
			out[key] = p.Head
		case "baseRefName":
			out[key] = p.Base
		case "url":
			out[key] = "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(p.Number)
		case "createdAt", "updatedAt":
			out[key] = epochStr()
		case "author":
			out[key] = e.resolveUser(viewerUser(), f.children, pth)
		case "files":
			paths := gqlPullFiles(repo, p)
			// files(first: N) honoured; default all.
			args := gqlResolveArgs(f, e.vars)
			if first, ok := args["first"]; ok {
				if n := gqlArgInt(args, "first", -1); n >= 0 && len(paths) > n {
					_ = first
					paths = paths[:n]
				}
			}
			nodes := []any{}
			for _, path := range paths {
				node := map[string]any{}
				for _, cf := range f.children {
					if cf.name == "nodes" {
						for _, nf := range cf.children {
							if nf.name == "path" {
								node[gqlFieldKey(nf)] = path
							} else {
								e.errf(append(append([]any{}, pth...), "nodes", gqlFieldKey(nf)), "Cannot query field %q on type %q", nf.name, "PullRequestChangedFile")
							}
						}
					}
				}
				// If the caller asked files{...} without a nodes child
				// (never in the checks), still emit the node map.
				if len(node) == 0 {
					node = map[string]any{"path": path}
				}
				nodes = append(nodes, node)
			}
			out[key] = map[string]any{"nodes": nodes, "totalCount": len(nodes)}
		default:
			e.errf(pth, "Cannot query field %q on type %q", f.name, "PullRequest")
		}
	}
	return out
}

// issuesConnection resolves repository.issues(...).
func (e *gqlExec) issuesConnection(repo *Repo, f *gqlField, path []any) map[string]any {
	args := gqlResolveArgs(f, e.vars)
	states := gqlArgStrList(args, "states")
	var want map[string]bool
	if len(states) > 0 {
		want = map[string]bool{}
		for _, st := range states {
			want[strings.ToLower(st)] = true
		}
	}
	desc := true
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d, _ := ob["direction"].(string); d == "ASC" {
			desc = false
		}
	}
	var ordered []*Issue
	if desc {
		for i := len(repo.Issues) - 1; i >= 0; i-- {
			ordered = append(ordered, repo.Issues[i])
		}
	} else {
		ordered = append(ordered, repo.Issues...)
	}
	var filtered []*Issue
	for _, iss := range ordered {
		if want != nil && !want[iss.State] {
			continue
		}
		filtered = append(filtered, iss)
	}
	start := 0
	if after := gqlArgStr(args, "after"); after != "" {
		if n, ok := gqlCursorNumber(after); ok {
			for i, iss := range filtered {
				if iss.Number == n {
					start = i + 1
					break
				}
			}
		}
	}
	first := gqlArgInt(args, "first", -1)
	page := filtered[start:]
	if first >= 0 && len(page) > first {
		page = page[:first]
	}
	hasNext := start+len(page) < len(filtered)
	endCursor := ""
	if len(page) > 0 {
		endCursor = gqlCursor(page[len(page)-1].Number)
	}
	startCursor := ""
	if len(page) > 0 {
		startCursor = gqlCursor(page[0].Number)
	}
	out := map[string]any{}
	for _, cf := range f.children {
		key := gqlFieldKey(cf)
		p := append(append([]any{}, path...), key)
		switch cf.name {
		case "nodes":
			nodes := []any{}
			for _, iss := range page {
				nodes = append(nodes, e.resolveIssue(repo, iss, cf.children, p))
			}
			out[key] = nodes
		case "edges":
			edges := []any{}
			for _, iss := range page {
				edges = append(edges, map[string]any{
					"node":   e.resolveIssue(repo, iss, cf.children, p),
					"cursor": gqlCursor(iss.Number),
				})
			}
			out[key] = edges
		case "pageInfo":
			pi := map[string]any{}
			for _, pf := range cf.children {
				pk := gqlFieldKey(pf)
				switch pf.name {
				case "hasNextPage":
					pi[pk] = hasNext
				case "hasPreviousPage":
					pi[pk] = start > 0
				case "endCursor":
					pi[pk] = endCursor
				case "startCursor":
					pi[pk] = startCursor
				default:
					e.errf(append(append([]any{}, p...), pk), "Cannot query field %q on type %q", pf.name, "PageInfo")
				}
			}
			out[key] = pi
		case "totalCount":
			out[key] = len(filtered)
		default:
			e.errf(p, "Cannot query field %q on type %q", cf.name, "IssueConnection")
		}
	}
	return out
}

// pullRequestsConnection resolves repository.pullRequests(first:).
func (e *gqlExec) pullRequestsConnection(repo *Repo, f *gqlField, path []any) map[string]any {
	args := gqlResolveArgs(f, e.vars)
	first := gqlArgInt(args, "first", -1)
	// gh pr list defaults to open pull requests, newest first.
	var ordered []*Pull
	for i := len(repo.Pulls) - 1; i >= 0; i-- {
		if repo.Pulls[i].State == "open" {
			ordered = append(ordered, repo.Pulls[i])
		}
	}
	page := ordered
	if first >= 0 && len(page) > first {
		page = page[:first]
	}
	hasNext := len(page) < len(ordered)
	endCursor := ""
	if len(page) > 0 {
		endCursor = gqlCursor(page[len(page)-1].Number)
	}
	out := map[string]any{}
	for _, cf := range f.children {
		key := gqlFieldKey(cf)
		p := append(append([]any{}, path...), key)
		switch cf.name {
		case "nodes":
			nodes := []any{}
			for _, pr := range page {
				nodes = append(nodes, e.resolvePull(repo, pr, cf.children, p))
			}
			out[key] = nodes
		case "edges":
			edges := []any{}
			for _, pr := range page {
				edges = append(edges, map[string]any{
					"node":   e.resolvePull(repo, pr, cf.children, p),
					"cursor": gqlCursor(pr.Number),
				})
			}
			out[key] = edges
		case "pageInfo":
			pi := map[string]any{}
			for _, pf := range cf.children {
				pk := gqlFieldKey(pf)
				switch pf.name {
				case "hasNextPage":
					pi[pk] = hasNext
				case "hasPreviousPage":
					pi[pk] = false
				case "endCursor":
					pi[pk] = endCursor
				case "startCursor":
					pi[pk] = ""
				default:
					e.errf(append(append([]any{}, p...), pk), "Cannot query field %q on type %q", pf.name, "PageInfo")
				}
			}
			out[key] = pi
		case "totalCount":
			out[key] = len(ordered)
		default:
			e.errf(p, "Cannot query field %q on type %q", cf.name, "PullRequestConnection")
		}
	}
	return out
}

// resolveRepo resolves a Repository selection set. Caller holds at least a
// read lock on s.mu (query path) or the repo is stable (mutation path).
func (e *gqlExec) resolveRepo(repo *Repo, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if f.fragOn != "" {
			if f.fragOn == "Repository" || f.fragOn == "Node" {
				for k, v := range e.resolveRepo(repo, f.children, path) {
					out[k] = v
				}
			}
			continue
		}
		key := gqlFieldKey(f)
		p := append(append([]any{}, path...), key)
		switch f.name {
		case "id":
			out[key] = gqlGlobalID("Repository", itoa(repo.ID))
		case "databaseId":
			out[key] = repo.ID
		case "name":
			out[key] = repo.Name
		case "nameWithOwner":
			out[key] = repo.Owner + "/" + repo.Name
		case "owner":
			owner := &User{Login: repo.Owner}
			if repo.Owner == ViewerLogin {
				owner = viewerUser()
			}
			out[key] = e.resolveUser(owner, f.children, p)
		case "defaultBranchRef":
			ref := map[string]any{}
			for _, rf := range f.children {
				rk := gqlFieldKey(rf)
				switch rf.name {
				case "name":
					ref[rk] = repo.DefaultBranch
				case "prefix":
					ref[rk] = "refs/heads/"
				case "id":
					ref[rk] = gqlGlobalID("Ref", itoa(repo.ID), repo.DefaultBranch)
				default:
					e.errf(append(append([]any{}, p...), rk), "Cannot query field %q on type %q", rf.name, "Ref")
				}
			}
			if len(ref) == 0 {
				ref = map[string]any{"name": repo.DefaultBranch}
			}
			out[key] = ref
		case "viewerPermission":
			out[key] = "ADMIN"
		case "hasIssuesEnabled":
			out[key] = true
		case "description":
			out[key] = repo.Description
		case "private":
			out[key] = repo.Private
		case "url":
			out[key] = "https://example.invalid/" + repo.Owner + "/" + repo.Name
		case "createdAt", "updatedAt":
			out[key] = epochStr()
		case "issue":
			args := gqlResolveArgs(f, e.vars)
			number := gqlArgInt(args, "number", 0)
			var found *Issue
			for _, iss := range repo.Issues {
				if iss.Number == number {
					found = iss
					break
				}
			}
			if found == nil {
				out[key] = nil
			} else {
				out[key] = e.resolveIssue(repo, found, f.children, p)
			}
		case "issues":
			out[key] = e.issuesConnection(repo, f, p)
		case "pullRequests":
			out[key] = e.pullRequestsConnection(repo, f, p)
		case "pullRequest":
			args := gqlResolveArgs(f, e.vars)
			number := gqlArgInt(args, "number", 0)
			var found *Pull
			for _, pr := range repo.Pulls {
				if pr.Number == number {
					found = pr
					break
				}
			}
			if found == nil {
				out[key] = nil
			} else {
				out[key] = e.resolvePull(repo, found, f.children, p)
			}
		default:
			e.errf(p, "Cannot query field %q on type %q", f.name, "Repository")
		}
	}
	return out
}

// resolveType answers __type(name:) introspection.
func (e *gqlExec) resolveType(name string, fields []*gqlField, path []any) map[string]any {
	out := map[string]any{}
	kind := "OBJECT"
	if _, ok := gqlTypeEnums[name]; ok {
		kind = "ENUM"
	}
	for _, f := range fields {
		key := gqlFieldKey(f)
		p := append(append([]any{}, path...), key)
		switch f.name {
		case "name":
			out[key] = name
		case "kind":
			out[key] = kind
		case "description":
			out[key] = ""
		case "fields":
			names := gqlTypeFields[name]
			list := []any{}
			for _, n := range names {
				entry := map[string]any{}
				for _, sf := range f.children {
					if sf.name == "name" {
						entry[gqlFieldKey(sf)] = n
					} else if sf.name == "description" {
						entry[gqlFieldKey(sf)] = ""
					} else {
						e.errf(append(append([]any{}, p...), gqlFieldKey(sf)), "Cannot query field %q on type %q", sf.name, "__Field")
					}
				}
				if len(entry) == 0 {
					entry = map[string]any{"name": n}
				}
				list = append(list, entry)
			}
			out[key] = list
		case "enumValues":
			names := gqlTypeEnums[name]
			list := []any{}
			for _, n := range names {
				entry := map[string]any{}
				for _, sf := range f.children {
					if sf.name == "name" {
						entry[gqlFieldKey(sf)] = n
					} else if sf.name == "description" {
						entry[gqlFieldKey(sf)] = ""
					} else {
						e.errf(append(append([]any{}, p...), gqlFieldKey(sf)), "Cannot query field %q on type %q", sf.name, "__EnumValue")
					}
				}
				if len(entry) == 0 {
					entry = map[string]any{"name": n}
				}
				list = append(list, entry)
			}
			out[key] = list
		default:
			e.errf(p, "Cannot query field %q on type %q", f.name, "__Type")
		}
	}
	return out
}

// resolveNode answers node(id:).
func (e *gqlExec) resolveNode(id string, fields []*gqlField, path []any) any {
	kind, parts, ok := gqlDecodeGlobalID(id)
	if !ok {
		return nil
	}
	switch kind {
	case "Repository":
		if len(parts) < 1 {
			return nil
		}
		rid, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		// Query path holds a read lock; reuse the locked map directly.
		repo := e.s.repos[repoKeyByID(e.s, rid)]
		if repo == nil {
			return nil
		}
		out := map[string]any{"id": id}
		for _, f := range fields {
			if f.fragOn != "" {
				if f.fragOn == "Repository" {
					for k, v := range e.resolveRepo(repo, f.children, path) {
						out[k] = v
					}
				}
				continue
			}
			if f.name == "id" {
				out[gqlFieldKey(f)] = id
				continue
			}
			e.errf(append(append([]any{}, path...), gqlFieldKey(f)), "Cannot query field %q on type %q", f.name, "Node")
		}
		return out
	case "Issue":
		if len(parts) < 2 {
			return nil
		}
		rid, err1 := strconv.Atoi(parts[0])
		num, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		repo := e.s.repos[repoKeyByID(e.s, rid)]
		if repo == nil {
			return nil
		}
		var found *Issue
		for _, iss := range repo.Issues {
			if iss.Number == num {
				found = iss
				break
			}
		}
		if found == nil {
			return nil
		}
		out := map[string]any{"id": id}
		for _, f := range fields {
			if f.fragOn != "" {
				if f.fragOn == "Issue" {
					for k, v := range e.resolveIssue(repo, found, f.children, path) {
						out[k] = v
					}
				}
				continue
			}
			if f.name == "id" {
				out[gqlFieldKey(f)] = id
				continue
			}
			e.errf(append(append([]any{}, path...), gqlFieldKey(f)), "Cannot query field %q on type %q", f.name, "Node")
		}
		return out
	case "PullRequest":
		if len(parts) < 2 {
			return nil
		}
		rid, err1 := strconv.Atoi(parts[0])
		num, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		repo := e.s.repos[repoKeyByID(e.s, rid)]
		if repo == nil {
			return nil
		}
		var found *Pull
		for _, pr := range repo.Pulls {
			if pr.Number == num {
				found = pr
				break
			}
		}
		if found == nil {
			return nil
		}
		out := map[string]any{"id": id}
		for _, f := range fields {
			if f.fragOn != "" {
				if f.fragOn == "PullRequest" {
					for k, v := range e.resolvePull(repo, found, f.children, path) {
						out[k] = v
					}
				}
				continue
			}
			if f.name == "id" {
				out[gqlFieldKey(f)] = id
				continue
			}
			e.errf(append(append([]any{}, path...), gqlFieldKey(f)), "Cannot query field %q on type %q", f.name, "Node")
		}
		return out
	case "User":
		if len(parts) < 1 {
			return nil
		}
		uid, err := strconv.Atoi(parts[0])
		if err != nil {
			return nil
		}
		u := viewerUser()
		if uid != ViewerID {
			u = &User{ID: uid, Login: StrangerLogin}
		}
		out := map[string]any{"id": id}
		for _, f := range fields {
			if f.fragOn != "" {
				if f.fragOn == "User" {
					for k, v := range e.resolveUser(u, f.children, path) {
						out[k] = v
					}
				}
				continue
			}
			if f.name == "id" {
				out[gqlFieldKey(f)] = id
				continue
			}
			e.errf(append(append([]any{}, path...), gqlFieldKey(f)), "Cannot query field %q on type %q", f.name, "Node")
		}
		return out
	}
	return nil
}

// repoKeyByID finds a repo's map key by numeric id. Caller holds a lock.
func repoKeyByID(s *Store, id int) string {
	for k, r := range s.repos {
		if r.ID == id {
			return k
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Root dispatch: queries (caller holds a read lock) and mutations.
// ---------------------------------------------------------------------------

// execQueryRoot resolves one root field of a query operation.
func (e *gqlExec) execQueryRoot(f *gqlField, path []any) any {
	key := gqlFieldKey(f)
	p := append(append([]any{}, path...), key)
	switch f.name {
	case "viewer":
		return e.resolveUser(viewerUser(), f.children, p)
	case "repository":
		args := gqlResolveArgs(f, e.vars)
		owner := gqlArgStr(args, "owner")
		name := gqlArgStr(args, "name")
		repo := e.s.repos[repoKey(owner, name)]
		if repo == nil {
			return nil
		}
		return e.resolveRepo(repo, f.children, p)
	case "node":
		args := gqlResolveArgs(f, e.vars)
		return e.resolveNode(gqlArgStr(args, "id"), f.children, p)
	case "__type":
		args := gqlResolveArgs(f, e.vars)
		name := gqlArgStr(args, "name")
		if _, ok := gqlTypeFields[name]; !ok {
			if _, ok := gqlTypeEnums[name]; !ok {
				return nil
			}
		}
		return e.resolveType(name, f.children, p)
	case "search":
		return e.execSearch(f, p)
	default:
		e.errf(p, "Cannot query field %q on type %q", f.name, "Query")
		return nil
	}
}

// execSearch is a minimal search (no check in checks-graphql.ts uses it;
// gh's search is issue search by text). Returns matching open+closed issues.
func (e *gqlExec) execSearch(f *gqlField, path []any) map[string]any {
	args := gqlResolveArgs(f, e.vars)
	query := strings.ToLower(gqlArgStr(args, "query"))
	first := gqlArgInt(args, "first", 10)
	var matches []*Issue
	var matchRepos []*Repo
	for _, repo := range e.s.repos {
		if repo.Owner != ViewerLogin {
			continue
		}
		for _, iss := range repo.Issues {
			if query == "" || strings.Contains(strings.ToLower(iss.Title+" "+iss.Body), query) {
				matches = append(matches, iss)
				matchRepos = append(matchRepos, repo)
			}
		}
	}
	if first >= 0 && len(matches) > first {
		matches = matches[:first]
		matchRepos = matchRepos[:first]
	}
	out := map[string]any{}
	for _, cf := range f.children {
		key := gqlFieldKey(cf)
		p := append(append([]any{}, path...), key)
		switch cf.name {
		case "nodes":
			nodes := []any{}
			for i, iss := range matches {
				nodes = append(nodes, e.resolveIssue(matchRepos[i], iss, cf.children, p))
			}
			out[key] = nodes
		case "issueCount":
			out[key] = len(matches)
		case "pageInfo":
			pi := map[string]any{}
			for _, pf := range cf.children {
				switch pf.name {
				case "hasNextPage":
					pi[gqlFieldKey(pf)] = false
				case "hasPreviousPage":
					pi[gqlFieldKey(pf)] = false
				case "endCursor":
					pi[gqlFieldKey(pf)] = ""
				case "startCursor":
					pi[gqlFieldKey(pf)] = ""
				}
			}
			out[key] = pi
		default:
			e.errf(p, "Cannot query field %q on type %q", cf.name, "SearchResultItemConnection")
		}
	}
	return out
}

// execMutation resolves one root field of a mutation operation. Mutations
// manage their own locking via the gql* helpers / createPullInRepo /
// mergePullSquash; the caller must NOT hold s.mu here.
func (e *gqlExec) execMutation(f *gqlField, path []any) any {
	key := gqlFieldKey(f)
	p := append(append([]any{}, path...), key)
	args := gqlResolveArgs(f, e.vars)
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch f.name {
	case "createIssue":
		repoIDStr := gqlArgStr(input, "repositoryId")
		kind, parts, ok := gqlDecodeGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(parts) < 1 {
			e.errf(p, "createIssue: invalid repositoryId")
			return nil
		}
		repoID, err := strconv.Atoi(parts[0])
		if err != nil {
			e.errf(p, "createIssue: invalid repositoryId")
			return nil
		}
		title := gqlArgStr(input, "title")
		if title == "" {
			e.errf(p, "createIssue: title is required")
			return nil
		}
		body := gqlArgStr(input, "body")
		repo := gqlRepoByID(e.s, repoID)
		if repo == nil {
			e.errf(p, "createIssue: repository not found")
			return nil
		}
		iss := gqlAddIssueToRepo(e.s, repo, title, body)
		return e.mutationPayload(f, "issue", func(children []*gqlField, cp []any) any {
			return e.resolveIssue(repo, iss, children, cp)
		}, p)
	case "closeIssue":
		issueIDStr := gqlArgStr(input, "issueId")
		kind, parts, ok := gqlDecodeGlobalID(issueIDStr)
		if !ok || kind != "Issue" || len(parts) < 2 {
			e.errf(p, "closeIssue: invalid issueId")
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf(p, "closeIssue: invalid issueId")
			return nil
		}
		iss, apiErr := gqlCloseIssueByID(e.s, repoID, number)
		if apiErr != nil {
			e.errf(p, "closeIssue: %s", apiErr.Message)
			return nil
		}
		repo := gqlRepoByID(e.s, repoID)
		return e.mutationPayload(f, "issue", func(children []*gqlField, cp []any) any {
			return e.resolveIssue(repo, iss, children, cp)
		}, p)
	case "createPullRequest":
		repoIDStr := gqlArgStr(input, "repositoryId")
		kind, parts, ok := gqlDecodeGlobalID(repoIDStr)
		if !ok || kind != "Repository" || len(parts) < 1 {
			e.errf(p, "createPullRequest: invalid repositoryId")
			return nil
		}
		repoID, err := strconv.Atoi(parts[0])
		if err != nil {
			e.errf(p, "createPullRequest: invalid repositoryId")
			return nil
		}
		repo := gqlRepoByID(e.s, repoID)
		if repo == nil {
			e.errf(p, "createPullRequest: repository not found")
			return nil
		}
		title := gqlArgStr(input, "title")
		head := gqlArgStr(input, "headRefName")
		base := gqlArgStr(input, "baseRefName")
		body := gqlArgStr(input, "body")
		pr, apiErr := createPullInRepo(e.s, repo, title, body, head, base)
		if apiErr != nil {
			e.errf(p, "createPullRequest: %s", apiErr.Message)
			return nil
		}
		return e.mutationPayload(f, "pullRequest", func(children []*gqlField, cp []any) any {
			return e.resolvePull(repo, pr, children, cp)
		}, p)
	case "mergePullRequest":
		prIDStr := gqlArgStr(input, "pullRequestId")
		kind, parts, ok := gqlDecodeGlobalID(prIDStr)
		if !ok || kind != "PullRequest" || len(parts) < 2 {
			e.errf(p, "mergePullRequest: invalid pullRequestId")
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf(p, "mergePullRequest: invalid pullRequestId")
			return nil
		}
		repo := gqlRepoByID(e.s, repoID)
		if repo == nil {
			e.errf(p, "mergePullRequest: repository not found")
			return nil
		}
		e.s.mu.Lock()
		var pr *Pull
		for _, cand := range repo.Pulls {
			if cand.Number == number {
				pr = cand
				break
			}
		}
		if pr == nil {
			e.s.mu.Unlock()
			e.errf(p, "mergePullRequest: pull request not found")
			return nil
		}
		_, apiErr := mergePullSquash(repo, pr)
		e.s.mu.Unlock()
		if apiErr != nil {
			e.errf(p, "mergePullRequest: %s", apiErr.Message)
			return nil
		}
		return e.mutationPayload(f, "pullRequest", func(children []*gqlField, cp []any) any {
			return e.resolvePull(repo, pr, children, cp)
		}, p)
	case "addComment":
		subjectIDStr := gqlArgStr(input, "subjectId")
		kind, parts, ok := gqlDecodeGlobalID(subjectIDStr)
		if !ok || kind != "Issue" || len(parts) < 2 {
			e.errf(p, "addComment: invalid subjectId")
			return nil
		}
		repoID, err1 := strconv.Atoi(parts[0])
		number, err2 := strconv.Atoi(parts[1])
		if err1 != nil || err2 != nil {
			e.errf(p, "addComment: invalid subjectId")
			return nil
		}
		body := gqlArgStr(input, "body")
		if body == "" {
			e.errf(p, "addComment: body is required")
			return nil
		}
		repo, iss, c, apiErr := gqlAddCommentByID(e.s, repoID, number, body)
		if apiErr != nil {
			e.errf(p, "addComment: %s", apiErr.Message)
			return nil
		}
		out := map[string]any{}
		for _, cf := range f.children {
			ckey := gqlFieldKey(cf)
			cp := append(append([]any{}, p...), ckey)
			switch cf.name {
			case "commentEdge":
				node := map[string]any{}
				for _, ef := range cf.children {
					if ef.name == "node" {
						node[gqlFieldKey(ef)] = e.resolveComment(repo, iss, c, ef.children, append(append([]any{}, cp...), gqlFieldKey(ef)))
					} else {
						e.errf(append(append([]any{}, cp...), gqlFieldKey(ef)), "Cannot query field %q on type %q", ef.name, "IssueCommentEdge")
					}
				}
				out[ckey] = node
			case "clientMutationId":
				if v, ok := input["clientMutationId"]; ok {
					out[ckey] = v
				} else {
					out[ckey] = nil
				}
			default:
				e.errf(cp, "Cannot query field %q on type %q", cf.name, "AddCommentPayload")
			}
		}
		return out
	default:
		e.errf(p, "Cannot query field %q on type %q", f.name, "Mutation")
		return nil
	}
}

// mutationPayload resolves a single-object mutation payload field (issue /
// pullRequest) plus clientMutationId.
func (e *gqlExec) mutationPayload(f *gqlField, fieldName string, resolve func(children []*gqlField, cp []any) any, path []any) map[string]any {
	out := map[string]any{}
	for _, cf := range f.children {
		ckey := gqlFieldKey(cf)
		cp := append(append([]any{}, path...), ckey)
		switch cf.name {
		case fieldName:
			out[ckey] = resolve(cf.children, cp)
		case "clientMutationId":
			out[ckey] = nil
		default:
			e.errf(cp, "Cannot query field %q on type %q", cf.name, "MutationPayload")
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// One operation -> one response object.
// ---------------------------------------------------------------------------

type gqlRawOp struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func gqlExecuteOp(s *Store, op gqlRawOp) map[string]any {
	vars := op.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	ops, err := gqlParseDocument(op.Query)
	if err != nil {
		return map[string]any{
			"data":   nil,
			"errors": []map[string]any{{"message": "Syntax Error: " + err.Error()}},
		}
	}
	var doc *gqlOp
	if op.OperationName != "" {
		for _, o := range ops {
			if o.name == op.OperationName {
				doc = o
				break
			}
		}
		if doc == nil {
			return map[string]any{
				"data":   nil,
				"errors": []map[string]any{{"message": "Unknown operation named \"" + op.OperationName + "\""}},
			}
		}
	} else {
		doc = ops[0]
	}
	e := &gqlExec{s: s, vars: vars}
	data := map[string]any{}
	if doc.opType == "mutation" {
		for _, f := range doc.fields {
			data[gqlFieldKey(f)] = e.execMutation(f, []any{})
		}
	} else {
		s.mu.RLock()
		for _, f := range doc.fields {
			data[gqlFieldKey(f)] = e.execQueryRoot(f, []any{})
		}
		s.mu.RUnlock()
	}
	resp := map[string]any{"data": data}
	if len(e.errs) > 0 {
		resp["errors"] = e.errs
	}
	return resp
}

// ---------------------------------------------------------------------------
// HTTP wiring.
// ---------------------------------------------------------------------------

func registerGraphQL(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("/api/graphql", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			methodNotAllowed(w, s)
			return
		}
		if !s.requireAuth(w, r) {
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
		if err != nil {
			writeErr(w, s, Err(400, "Problems parsing JSON"))
			return
		}
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" {
			writeErr(w, s, Err(400, "Problems parsing JSON"))
			return
		}
		if strings.HasPrefix(trimmed, "[") {
			var ops []gqlRawOp
			if err := json.Unmarshal(body, &ops); err != nil {
				writeErr(w, s, Err(400, "Problems parsing JSON"))
				return
			}
			responses := make([]any, 0, len(ops))
			for _, op := range ops {
				responses = append(responses, gqlExecuteOp(s, op))
			}
			writeJSON(w, s, 200, responses)
			return
		}
		var op gqlRawOp
		if err := json.Unmarshal(body, &op); err != nil {
			writeErr(w, s, Err(400, "Problems parsing JSON"))
			return
		}
		writeJSON(w, s, 200, gqlExecuteOp(s, op))
	})
}
