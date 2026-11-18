// gql.go: GraphQL v4 at /api/graphql — parser and executor over the forge.
// Port of the entry's TS implementation (same documents, same behavior).
package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- parser

type Selection struct {
	Kind      string // field | fragment
	Name      string
	Alias     string
	Args      map[string]interface{}
	Selections []Selection
}

type Operation struct {
	Type      string
	Selections []Selection
}

type ParsedDoc struct {
	Operations []Operation
	Fragments  map[string][]Selection
}

type gqlParser struct {
	src string
	at  int
}

func parseDocument(query string) (*ParsedDoc, error) {
	p := &gqlParser{src: query}
	doc := &ParsedDoc{Fragments: map[string][]Selection{}}
	for {
		p.skip()
		if p.at >= len(p.src) {
			break
		}
		w := p.word()
		switch w {
		case "query", "mutation":
			p.skip()
			if p.at < len(p.src) && isWordStart(p.peek()) && p.peek() != '{' {
				p.word() // operation name
			}
			p.skip()
			if p.at < len(p.src) && p.peek() == '(' {
				p.skipParenAndVars()
			}
			p.skip()
			sels, err := p.selectionSetOpt()
			if err != nil {
				return nil, err
			}
			t := "query"
			if w == "mutation" {
				t = "mutation"
			}
			doc.Operations = append(doc.Operations, Operation{Type: t, Selections: sels})
		case "fragment":
			p.skip()
			fragName := p.word()
			p.skip()
			if strings.HasPrefix(p.src[p.at:], "on") {
				p.at += 2
				p.skip()
				p.word()
			}
			sels, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.Fragments[fragName] = sels
		case "":
			p.at++
		default:
			// shorthand operation: w itself is the first root field name
			var sels []Selection
			name := w
			alias := ""
			p.skip()
			if p.peek() == ':' {
				p.at++
				p.skip()
				alias = w
				name = p.word()
			}
			args := p.parseArguments()
			p.skip()
			var fieldSels []Selection
			if p.peek() == '{' {
				var err2 error
				fieldSels, err2 = p.selectionSet()
				if err2 != nil {
					return nil, err2
				}
			}
			sels = append(sels, Selection{Kind: "field", Name: name, Alias: alias, Args: args, Selections: fieldSels})
			for {
				p.skip()
				if p.at >= len(p.src) || p.peek() == '}' {
					break
				}
				fname := p.word()
				if fname == "" {
					p.at++
					continue
				}
				fAlias := ""
				fName := fname
				p.skip()
				if p.peek() == ':' {
					p.at++
					p.skip()
					fAlias = fname
					fName = p.word()
				}
				fArgs := p.parseArguments()
				p.skip()
				var fSels []Selection
				if p.peek() == '{' {
					var err3 error
					fSels, err3 = p.selectionSet()
					if err3 != nil {
						return nil, err3
					}
				}
				sels = append(sels, Selection{Kind: "field", Name: fName, Alias: fAlias, Args: fArgs, Selections: fSels})
			}
			doc.Operations = append(doc.Operations, Operation{Type: "query", Selections: sels})
		}
	}
	return doc, nil
}

func isWordStart(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func (p *gqlParser) peek() byte {
	if p.at < len(p.src) {
		return p.src[p.at]
	}
	return ' '
}

func (p *gqlParser) skip() {
	for p.at < len(p.src) {
		c := p.src[p.at]
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' || c == ',' {
			p.at++
		} else if c == '#' {
			for p.at < len(p.src) && p.src[p.at] != '\n' {
				p.at++
			}
		} else {
			break
		}
	}
}

func (p *gqlParser) word() string {
	start := p.at
	for p.at < len(p.src) && (isWordStart(p.src[p.at]) || (p.src[p.at] >= '0' && p.src[p.at] <= '9')) {
		p.at++
	}
	return p.src[start:p.at]
}

func (p *gqlParser) skipParenAndVars() {
	p.skip()
	if p.peek() != '(' {
		return
	}
	p.at++
	for {
		p.skip()
		if p.at >= len(p.src) || p.peek() == ')' {
			p.at++
			return
		}
		if p.peek() == '$' {
			p.at++
			p.word()
		}
		p.skip()
		if p.peek() == ':' {
			p.at++
		}
		p.skip()
		for p.at < len(p.src) && (isWordStart(p.src[p.at]) || (p.src[p.at] >= '0' && p.src[p.at] <= '9') || p.src[p.at] == '!' || p.src[p.at] == '[' || p.src[p.at] == ']') {
			p.at++
		}
		p.skip()
		if p.peek() == '=' {
			p.at++
			p.skip()
			p.parseValue()
		}
		p.skip()
		if p.peek() == ',' {
			p.at++
		}
	}
}

func (p *gqlParser) parseValue() interface{} {
	p.skip()
	if p.at >= len(p.src) {
		return nil
	}
	c := p.src[p.at]
	switch {
	case c == '"':
		p.at++
		var s strings.Builder
		for p.at < len(p.src) && p.src[p.at] != '"' {
			if p.src[p.at] == '\\' {
				p.at++
				if p.at >= len(p.src) {
					break
				}
				switch p.src[p.at] {
				case 'n':
					s.WriteByte('\n')
				case 't':
					s.WriteByte('\t')
				case 'u':
					if p.at+4 < len(p.src) {
						if v, err := strconv.ParseUint(p.src[p.at+1:p.at+5], 16, 32); err == nil {
							s.WriteRune(rune(v))
						}
						p.at += 4
					}
				default:
					s.WriteByte(p.src[p.at])
				}
				p.at++
			} else {
				s.WriteByte(p.src[p.at])
				p.at++
			}
		}
		p.at++
		return s.String()
	case c == '$':
		p.at++
		return map[string]interface{}{"__var": p.word()}
	case c == '[':
		p.at++
		var list []interface{}
		p.skip()
		for p.at < len(p.src) && p.src[p.at] != ']' {
			list = append(list, p.parseValue())
			p.skip()
			if p.at < len(p.src) && p.src[p.at] == ',' {
				p.at++
			}
			p.skip()
		}
		p.at++
		return list
	case c == '{':
		p.at++
		obj := map[string]interface{}{}
		p.skip()
		for p.at < len(p.src) && p.src[p.at] != '}' {
			p.skip()
			key := p.word()
			p.skip()
			if p.peek() == ':' {
				p.at++
			}
			obj[key] = p.parseValue()
			p.skip()
			if p.at < len(p.src) && p.src[p.at] == ',' {
				p.at++
			}
			p.skip()
		}
		p.at++
		return obj
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.at
		for p.at < len(p.src) && (p.src[p.at] == '-' || p.src[p.at] == '.' || (p.src[p.at] >= '0' && p.src[p.at] <= '9')) {
			p.at++
		}
		if v, err := strconv.ParseFloat(p.src[start:p.at], 64); err == nil {
			return v
		}
		return 0
	default:
		return map[string]interface{}{"__enum": p.word()}
	}
}

func (p *gqlParser) parseArguments() map[string]interface{} {
	args := map[string]interface{}{}
	p.skip()
	if p.peek() != '(' {
		return args
	}
	p.at++
	for {
		p.skip()
		if p.at >= len(p.src) || p.peek() == ')' {
			p.at++
			return args
		}
		name := p.word()
		p.skip()
		if p.peek() == ':' {
			p.at++
		}
		args[name] = p.parseValue()
		p.skip()
		if p.peek() == ',' {
			p.at++
		}
	}
}

func (p *gqlParser) selectionSet() ([]Selection, error) {
	p.skip()
	if p.peek() != '{' {
		return nil, fmt.Errorf("expected { at %d", p.at)
	}
	p.at++
	var out []Selection
	for {
		p.skip()
		if p.at >= len(p.src) {
			return nil, fmt.Errorf("unterminated selection set")
		}
		if p.peek() == '}' {
			p.at++
			return out, nil
		}
		if strings.HasPrefix(p.src[p.at:], "...") {
			p.at += 3
			p.skip()
			w := p.word()
			if w == "on" {
				p.skip()
				p.word()
				p.skip()
				if p.peek() == '{' {
					inner, err := p.selectionSet()
					if err != nil {
						return nil, err
					}
					out = append(out, inner...)
				}
			} else {
				out = append(out, Selection{Kind: "fragment", Name: w})
			}
			continue
		}
		nameOrAlias := p.word()
		alias := ""
		name := nameOrAlias
		p.skip()
		if p.peek() == ':' {
			p.at++
			p.skip()
			alias = nameOrAlias
			name = p.word()
		}
		args := p.parseArguments()
		p.skip()
		var sels []Selection
		if p.peek() == '{' {
			var err error
			sels, err = p.selectionSet()
			if err != nil {
				return nil, err
			}
		}
		out = append(out, Selection{Kind: "field", Name: name, Alias: alias, Args: args, Selections: sels})
	}
}

func (p *gqlParser) selectionSetOpt() ([]Selection, error) {
	p.skip()
	if p.peek() == '{' {
		return p.selectionSet()
	}
	return nil, nil
}

// ---------------------------------------------------------------- executor

type GQLErrorEntry struct {
	Message string `json:"message"`
}

type GraphQLResult struct {
	Data   map[string]interface{} `json:"data"`
	Errors []GQLErrorEntry        `json:"errors,omitempty"`
}

type gqlNode struct {
	typeName string
	resolve  func(field string, args map[string]interface{}, sels []Selection) interface{}
}

func deepResolveVars(value interface{}, vars map[string]interface{}) interface{} {
	switch v := value.(type) {
	case map[string]interface{}:
		if varName, ok := v["__var"]; ok {
			if got, found := vars[fmt.Sprint(varName)]; found {
				return got
			}
			return nil
		}
		if _, ok := v["__enum"]; ok {
			return v
		}
		out := map[string]interface{}{}
		for k, val := range v {
			out[k] = deepResolveVars(val, vars)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, x := range v {
			out[i] = deepResolveVars(x, vars)
		}
		return out
	}
	return value
}

func enumStr(v interface{}) string {
	if m, ok := v.(map[string]interface{}); ok {
		if e, ok := m["__enum"]; ok {
			return fmt.Sprint(e)
		}
	}
	return fmt.Sprint(v)
}

func issueNode(forge *Forge, repo *Repo, issue *Issue) gqlNode {
	return gqlNode{typeName: "Issue", resolve: func(field string, args map[string]interface{}, sels []Selection) interface{} {
		switch field {
		case "id":
			return NodeID("Issue", issue.ID)
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "body":
			return issue.Body
		case "state":
			return strings.ToUpper(issue.State)
		case "author", "user":
			return gqlNode{typeName: "User", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "login" {
					return ViewerLogin
				}
				if f == "id" {
					return NodeID("User", ViewerID)
				}
				return nil
			}}
		case "comments":
			return commentsNode(len(issue.Comments))
		case "createdAt":
			return issue.CreatedAt
		case "closedAt":
			return issue.ClosedAt
		case "repository":
			return repositoryNode(forge, repo)
		}
		return nil
	}}
}

func commentsNode(count int) gqlNode {
	return gqlNode{typeName: "IssueComments", resolve: func(field string, args map[string]interface{}, sels []Selection) interface{} {
		if field == "totalCount" {
			return count
		}
		if field == "pageInfo" {
			return pageInfoNode(false, "")
		}
		return nil
	}}
}

func pageInfoNode(hasNext bool, endCursor string) gqlNode {
	return gqlNode{typeName: "PageInfo", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
		switch f {
		case "hasNextPage":
			return hasNext
		case "hasPreviousPage":
			return false
		case "endCursor":
			if endCursor == "" {
				return nil
			}
			return endCursor
		}
		return nil
	}}
}

func pullFileRows(forge *Forge, repo *Repo, pull *Pull) []DiffRow {
	baseTip := repo.Refs["refs/heads/"+pull.BaseRef]
	if baseTip == "" {
		baseTip = pull.BaseSha
	}
	headTip := repo.Refs["refs/heads/"+pull.HeadRef]
	if headTip == "" {
		headTip = pull.HeadSha
	}
	baseView := forge.CommitView(repo, baseTip)
	headView := forge.CommitView(repo, headTip)
	if baseView == nil || headView == nil {
		return nil
	}
	return forge.DiffTrees(repo, forge.FilesAtTree(repo, baseView.Tree, "", nil), forge.FilesAtTree(repo, headView.Tree, "", nil))
}

func pullNode(pull *Pull, repo *Repo, forge *Forge) gqlNode {
	return gqlNode{typeName: "PullRequest", resolve: func(field string, args map[string]interface{}, sels []Selection) interface{} {
		switch field {
		case "id":
			return NodeID("PullRequest", pull.ID)
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "body":
			return pull.Body
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			return strings.ToUpper(pull.State)
		case "merged":
			return pull.Merged
		case "mergedAt":
			return pull.MergedAt
		case "headRefName":
			return pull.HeadRef
		case "headRefOid":
			if sha := repo.Refs["refs/heads/"+pull.HeadRef]; sha != "" {
				return sha
			}
			return pull.HeadSha
		case "baseRefName":
			return pull.BaseRef
		case "mergeable":
			return "MERGEABLE"
		case "author":
			return gqlNode{typeName: "User", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "login" {
					return ViewerLogin
				}
				return nil
			}}
		case "files":
			rows := pullFileRows(forge, repo, pull)
			return filesConnection(rows)
		}
		return nil
	}}
}

func filesConnection(rows []DiffRow) gqlNode {
	return gqlNode{typeName: "PullRequestFilesConnection", resolve: func(field string, args map[string]interface{}, sels []Selection) interface{} {
		if field == "totalCount" {
			return len(rows)
		}
		if field == "edges" || field == "nodes" {
			var out []interface{}
			for _, row := range rows {
				fileNode := gqlNode{typeName: "PullRequestChangedFile", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
					switch f {
					case "path":
						return row.Filename
					case "additions":
						return row.Additions
					case "deletions":
						return row.Deletions
					case "changeType":
						return strings.ToUpper(row.Status)
					}
					return nil
				}}
				if field == "nodes" {
					out = append(out, fileNode)
				} else {
					out = append(out, gqlNode{typeName: "PullRequestFileEdge", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
						if f == "node" {
							return fileNode
						}
						return nil
					}})
				}
			}
			return out
		}
		if field == "pageInfo" {
			return pageInfoNode(false, "")
		}
		return nil
	}}
}

func repositoryNode(forge *Forge, repo *Repo) gqlNode {
	return gqlNode{typeName: "Repository", resolve: func(field string, args map[string]interface{}, sels []Selection) interface{} {
		switch field {
		case "id":
			return NodeID("Repository", repo.ID)
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name
		case "description":
			return repo.Description
		case "owner":
			return gqlNode{typeName: "User", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "login" {
					return repo.Owner
				}
				if f == "id" {
					return NodeID("User", ViewerID)
				}
				return nil
			}}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "defaultBranchRef":
			tip, ok := forge.HeadCommit(repo, "")
			if !ok {
				return nil
			}
			return gqlNode{typeName: "Ref", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "name" {
					return repo.DefaultBranch
				}
				if f == "target" {
					return commitNode(repo, tip)
				}
				return nil
			}}
		case "issue":
			if num, ok := toInt(args["number"]); ok {
				if issue, found := repo.Issues[num]; found {
					return issueNode(forge, repo, issue)
				}
			}
			return nil
		case "issues":
			first := 10
			if v, ok := toInt(args["first"]); ok {
				first = v
			}
			after := -1
			if s, ok := args["after"].(string); ok {
				after = parseCursor(s)
			}
			var issues []*Issue
			for _, i := range repo.Issues {
				if !i.IsPull {
					issues = append(issues, i)
				}
			}
			if statesRaw, ok := args["states"].([]interface{}); ok {
				wanted := map[string]bool{}
				for _, s := range statesRaw {
					wanted[strings.ToUpper(enumStr(s))] = true
				}
				filtered := issues[:0]
				for _, i := range issues {
					if wanted[strings.ToUpper(i.State)] {
						filtered = append(filtered, i)
					}
				}
				issues = filtered
			}
			desc := true
			if order, ok := args["orderBy"].(map[string]interface{}); ok {
				if strings.EqualFold(enumStr(order["direction"]), "ASC") {
					desc = false
				}
			}
			sort.Slice(issues, func(i, j int) bool {
				if desc {
					return issues[i].Number > issues[j].Number
				}
				return issues[i].Number < issues[j].Number
			})
			start := 0
			if after >= 0 {
				start = after
			}
			end := start + first
			if end > len(issues) {
				end = len(issues)
			}
			var slice []interface{}
			for _, issue := range issues[start:end] {
				slice = append(slice, issueNode(forge, repo, issue))
			}
			hasNext := end < len(issues)
			nextCursor := ""
			if hasNext {
				nextCursor = "cursor" + strconv.Itoa(end)
			}
			return gqlNode{typeName: "IssueConnection", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				switch f {
				case "totalCount":
					return len(issues)
				case "nodes":
					return slice
				case "edges":
					return slice
				case "pageInfo":
					return pageInfoNode(hasNext, nextCursor)
				}
				return nil
			}}
		case "pullRequest":
			if num, ok := toInt(args["number"]); ok {
				if pull, found := repo.Pulls[num]; found {
					return pullNode(pull, repo, forge)
				}
			}
			return nil
		case "pullRequests":
			first := 5
			if v, ok := toInt(args["first"]); ok {
				first = v
			}
			var pulls []*Pull
			for _, p := range repo.Pulls {
				pulls = append(pulls, p)
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			if len(pulls) > first {
				pulls = pulls[:first]
			}
			var slice []interface{}
			for _, pull := range pulls {
				slice = append(slice, pullNode(pull, repo, forge))
			}
			return gqlNode{typeName: "PullRequestConnection", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				switch f {
				case "totalCount":
					return len(repo.Pulls)
				case "nodes", "edges":
					return slice
				case "pageInfo":
					return pageInfoNode(false, "")
				}
				return nil
			}}
		case "createdAt":
			return repo.CreatedAt
		case "isPrivate":
			return repo.Private
		}
		return nil
	}}
}

func commitNode(repo *Repo, oid string) gqlNode {
	return gqlNode{typeName: "Commit", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
		if f == "oid" {
			return oid
		}
		return nil
	}}
}

func parseCursor(s string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "cursor"))
	if err != nil {
		return -1
	}
	return n
}

func toInt(v interface{}) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	return 0, false
}

// introspection

var introspectionTypes = map[string][]struct{ name, kind string }{
	"Issue":       {{"id", "ID"}, {"number", "Int"}, {"title", "String"}, {"state", "IssueState"}, {"body", "String"}, {"author", "Actor"}, {"comments", "IssueCommentsConnection"}, {"createdAt", "DateTime"}, {"closedAt", "DateTime"}, {"repository", "Repository"}},
	"PullRequest": {{"id", "ID"}, {"number", "Int"}, {"title", "String"}, {"state", "PullRequestState"}, {"body", "String"}, {"headRefName", "String"}, {"baseRefName", "String"}, {"mergeable", "MergeableState"}, {"merged", "Boolean"}, {"files", "PullRequestFilesConnection"}, {"author", "Actor"}, {"createdAt", "DateTime"}},
	"Repository":  {{"id", "ID"}, {"databaseId", "Int"}, {"name", "String"}, {"nameWithOwner", "String"}, {"owner", "RepositoryOwner"}, {"defaultBranchRef", "Ref"}, {"viewerPermission", "RepositoryPermission"}, {"hasIssuesEnabled", "Boolean"}, {"issue", "Issue"}, {"issues", "IssueConnection"}, {"pullRequests", "PullRequestConnection"}, {"pullRequest", "PullRequest"}, {"description", "String"}, {"createdAt", "DateTime"}},
}

func introspectionResult(args map[string]interface{}) interface{} {
	name, _ := args["name"].(string)
	if name == "SearchType" {
		return gqlNode{typeName: "__Type", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
			if f == "name" {
				return "SearchType"
			}
			if f == "kind" {
				return "ENUM"
			}
			if f == "enumValues" {
				return []interface{}{gqlNode{typeName: "__EnumValue", resolve: func(g string, a map[string]interface{}, s []Selection) interface{} {
					if g == "name" {
						return "ISSUE"
					}
					if g == "description" {
						return "Issues"
					}
					return nil
				}}}
			}
			return nil
		}}
	}
	fields, ok := introspectionTypes[name]
	if !ok {
		return nil
	}
	return gqlNode{typeName: "__Type", resolve: func(field string, a map[string]interface{}, s []Selection) interface{} {
		switch field {
		case "name":
			return name
		case "kind":
			return "OBJECT"
		case "fields":
			var out []interface{}
			for _, f := range fields {
				fd := f
				out = append(out, gqlNode{typeName: "__Field", resolve: func(g string, a map[string]interface{}, s []Selection) interface{} {
					switch g {
					case "name":
						return fd.name
					case "type":
						return typeRef(fd.kind)
					case "args":
						return []interface{}{}
					}
					return nil
				}})
			}
			return out
		}
		return nil
	}}
}

func typeRef(kind string) interface{} {
	return gqlNode{typeName: "__Type", resolve: func(field string, a map[string]interface{}, s []Selection) interface{} {
		if field == "name" {
			return kind
		}
		if field == "kind" {
			return "SCALAR"
		}
		if field == "ofType" {
			return nil
		}
		return nil
	}}
}

type GraphQLRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

// ExecuteGraphQL runs one request against the forge.
func ExecuteGraphQL(forge *Forge, req GraphQLRequest) GraphQLResult {
	doc, err := parseDocument(req.Query)
	if os.Getenv("FORGE_DEBUG") != "" {
		b, _ := json.Marshal(doc)
		os.Stderr.WriteString("[gql] parsed: " + string(b) + "\n")
	}
	if err != nil {
		return GraphQLResult{Data: nil, Errors: []GQLErrorEntry{{Message: "Parse error: " + err.Error()}}}
	}
	if len(doc.Operations) == 0 {
		return GraphQLResult{Data: nil, Errors: []GQLErrorEntry{{Message: "No operation"}}}
	}
	op := doc.Operations[0]
	vars := req.Variables
	if vars == nil {
		vars = map[string]interface{}{}
	}
	var errors []GQLErrorEntry
	data := map[string]interface{}{}
	var shapeValue func(value interface{}, sels []Selection, path string) interface{}
	var runSelections func(node *gqlNode, sels []Selection, target map[string]interface{}, path string)
	resolveArgs := func(args map[string]interface{}) map[string]interface{} {
		out := map[string]interface{}{}
		for k, v := range args {
			out[k] = deepResolveVars(v, vars)
		}
		return out
	}
	runSelections = func(node *gqlNode, sels []Selection, target map[string]interface{}, path string) {
		for _, sel := range sels {
			if sel.Kind == "fragment" {
				if frag, ok := doc.Fragments[sel.Name]; ok {
					runSelections(node, frag, target, path)
				}
				continue
			}
			alias := sel.Alias
			if alias == "" {
				alias = sel.Name
			}
			if sel.Name == "__typename" {
				if node != nil {
					target[alias] = node.typeName
				} else {
					target[alias] = nil
				}
				continue
			}
			if node == nil {
				target[alias] = nil
				continue
			}
			resolved := node.resolve(sel.Name, resolveArgs(sel.Args), sel.Selections)
			if resolved == nil {
				if sel.Name != "closedAt" && !isNullPossible(node.typeName, sel.Name) {
					errors = append(errors, GQLErrorEntry{Message: fmt.Sprintf("Field '%s' doesn't exist on type '%s'", sel.Name, node.typeName)})
				}
				target[alias] = nil
				continue
			}
			target[alias] = shapeValue(resolved, sel.Selections, path+"/"+sel.Name)
		}
	}
	shapeValue = func(value interface{}, sels []Selection, path string) interface{} {
		if value == nil {
			return nil
		}
		if list, ok := value.([]interface{}); ok {
			var out []interface{}
			for _, v := range list {
				out = append(out, shapeValue(v, sels, path))
			}
			return out
		}
		if node, ok := value.(gqlNode); ok {
			if len(sels) == 0 {
				return map[string]interface{}{}
			}
			out := map[string]interface{}{}
			runSelections(&node, sels, out, path)
			return out
		}
		// mutation payload pair {node, result}
		if pair, ok := value.(mutationPayload); ok {
			return shapeValue(pair.result, sels, path)
		}
		if len(sels) == 0 {
			return value
		}
		if m, ok := value.(map[string]interface{}); ok {
			out := map[string]interface{}{}
			for _, s := range sels {
				if s.Kind == "field" {
					alias := s.Alias
					if alias == "" {
						alias = s.Name
					}
					out[alias] = shapeValue(m[s.Name], s.Selections, path)
				}
			}
			return out
		}
		return value
	}
	runMutationField := func(field string, args map[string]interface{}) interface{} {
		a := resolveArgs(args)
		if input, ok := a["input"].(map[string]interface{}); ok {
			a = input
		}
		switch field {
		case "createIssue":
			repo := findRepo(forge, a["repositoryId"])
			if repo == nil {
				return nil
			}
			title, _ := a["title"].(string)
			bodyText, _ := a["body"].(string)
			number := repo.NextIssueNumber
			repo.NextIssueNumber++
			issue := &Issue{ID: 9000 + number, Number: number, Title: title, Body: bodyText, State: "open", CreatedAt: NowISO()}
			repo.Issues[number] = issue
			node := issueNode(forge, repo, issue)
			return mutationPayload{result: gqlNode{typeName: "CreateIssuePayload", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "issue" {
					return node
				}
				return nil
			}}}
		case "closeIssue":
			found := findIssueById(forge, strVal(a["issueId"]))
			if found == nil {
				return nil
			}
			found.issue.State = "closed"
			found.issue.ClosedAt = NowISO()
			node := issueNode(forge, found.repo, found.issue)
			return mutationPayload{result: gqlNode{typeName: "CloseIssuePayload", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "issue" {
					return node
				}
				return nil
			}}}
		case "createPullRequest":
			repo := findRepo(forge, a["repositoryId"])
			if repo == nil {
				return nil
			}
			headRef, _ := a["headRefName"].(string)
			baseRef, _ := a["baseRefName"].(string)
			title, _ := a["title"].(string)
			headSha := repo.Refs["refs/heads/"+headRef]
			baseSha := repo.Refs["refs/heads/"+baseRef]
			if headSha == "" || baseSha == "" {
				return nil
			}
			number := repo.NextIssueNumber
			repo.NextIssueNumber++
			pull := &Pull{ID: 9500 + number, Number: number, Title: title, State: "open",
				HeadRef: headRef, HeadSha: headSha, BaseRef: baseRef, BaseSha: baseSha,
				MergedBy: ViewerLogin, CreatedAt: NowISO()}
			repo.Pulls[number] = pull
			node := pullNode(pull, repo, forge)
			return mutationPayload{result: gqlNode{typeName: "CreatePullRequestPayload", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "pullRequest" {
					return node
				}
				return nil
			}}}
		case "mergePullRequest":
			found := findPullById(forge, strVal(a["pullRequestId"]))
			if found == nil {
				return nil
			}
			result := forge.MergePull(found.repo, found.pull, "squash")
			if !result.OK {
				return nil
			}
			node := pullNode(found.pull, found.repo, forge)
			return mutationPayload{result: gqlNode{typeName: "MergePullRequestPayload", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "pullRequest" {
					return node
				}
				return nil
			}}}
		case "addComment":
			found := findIssueById(forge, strVal(a["subjectId"]))
			if found == nil {
				return nil
			}
			body, _ := a["body"].(string)
			comment := Comment{ID: found.repo.NextCommentID, Body: body, Login: ViewerLogin, CreatedAt: NowISO()}
			found.repo.NextCommentID++
			found.issue.Comments = append(found.issue.Comments, comment)
			commentNode := gqlNode{typeName: "IssueComment", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "id" {
					return NodeID("IssueComment", comment.ID)
				}
				if f == "body" {
					return comment.Body
				}
				return nil
			}}
			return mutationPayload{result: gqlNode{typeName: "AddCommentPayload", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "commentEdge" {
					return gqlNode{typeName: "CommentEdge", resolve: func(g string, a map[string]interface{}, s []Selection) interface{} {
						if g == "node" {
							return commentNode
						}
						return nil
					}}
				}
				return nil
			}}}
		}
		return nil
	}
	for _, sel := range op.Selections {
		if sel.Kind != "field" {
			continue
		}
		alias := sel.Alias
		if alias == "" {
			alias = sel.Name
		}
		if op.Type == "mutation" {
			mutated := runMutationField(sel.Name, sel.Args)
			if mutated == nil {
				errors = append(errors, GQLErrorEntry{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Mutation'", sel.Name)})
				data[alias] = nil
				continue
			}
			data[alias] = shapeValue(mutated, sel.Selections, alias)
			continue
		}
		switch sel.Name {
		case "viewer":
			viewer := gqlNode{typeName: "User", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "login" {
					return ViewerLogin
				}
				if f == "id" {
					return NodeID("User", ViewerID)
				}
				return nil
			}}
			data[alias] = shapeValue(viewer, sel.Selections, alias)
		case "repository":
			a := resolveArgs(sel.Args)
			owner, _ := a["owner"].(string)
			name, _ := a["name"].(string)
			repo := forge.GetRepo(owner, name)
			if repo == nil || !strings.EqualFold(owner, ViewerLogin) {
				data[alias] = nil
				break
			}
			node := repositoryNode(forge, repo)
			data[alias] = shapeValue(node, sel.Selections, alias)
		case "node":
			a := resolveArgs(sel.Args)
			id, _ := a["id"].(string)
			if node := nodeById(forge, id); node != nil {
				data[alias] = shapeValue(node, sel.Selections, alias)
			} else {
				data[alias] = nil
			}
		case "__schema":
			node := gqlNode{typeName: "__Schema", resolve: func(f string, a map[string]interface{}, s []Selection) interface{} {
				if f == "queryType" {
					return typeRef("Query")
				}
				return nil
			}}
			data[alias] = shapeValue(node, sel.Selections, alias)
		case "__type":
			data[alias] = shapeValue(introspectionResult(resolveArgs(sel.Args)), sel.Selections, alias)
		default:
			errors = append(errors, GQLErrorEntry{Message: fmt.Sprintf("Field '%s' doesn't exist on type 'Query'", sel.Name)})
			data[alias] = nil
		}
	}
	if len(errors) > 0 {
		return GraphQLResult{Data: data, Errors: errors}
	}
	return GraphQLResult{Data: data}
}

func isNullPossible(typeName, field string) bool {
	// fields that legitimately resolve to null without being errors
	return field == "closedAt" || field == "endCursor" || field == "ofType" || field == "mergedAt" ||
		field == "description" || field == "databaseId" || field == "name"
}

type mutationPayload struct{ result interface{} }

func strVal(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func findRepo(forge *Forge, repositoryId interface{}) *Repo {
	id := strVal(repositoryId)
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	s := string(decoded)
	var dbID int
	if _, err := fmtSscan(s, "010:Repository", &dbID); err != nil {
		return nil
	}
	for _, repo := range forge.repos {
		if repo.ID == dbID {
			return repo
		}
	}
	return nil
}

func fmtSscan(s, prefix string, out *int) (bool, error) {
	if !strings.HasPrefix(s, prefix) {
		return false, fmt.Errorf("no match")
	}
	n, err := strconv.Atoi(s[len(prefix):])
	if err != nil {
		return false, err
	}
	*out = n
	return true, nil
}

type issueHit struct {
	repo  *Repo
	issue *Issue
}

func findIssueById(forge *Forge, id string) *issueHit {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	s := string(decoded)
	if !strings.HasPrefix(s, "010:Issue") {
		return nil
	}
	dbID, err := strconv.Atoi(s[len("010:Issue"):])
	if err != nil {
		return nil
	}
	for _, repo := range forge.repos {
		for _, issue := range repo.Issues {
			if issue.ID == dbID {
				return &issueHit{repo: repo, issue: issue}
			}
		}
	}
	return nil
}

type pullHit struct {
	repo *Repo
	pull *Pull
}

func findPullById(forge *Forge, id string) *pullHit {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	s := string(decoded)
	if !strings.HasPrefix(s, "010:PullRequest") {
		return nil
	}
	dbID, err := strconv.Atoi(s[len("010:PullRequest"):])
	if err != nil {
		return nil
	}
	for _, repo := range forge.repos {
		for _, pull := range repo.Pulls {
			if pull.ID == dbID {
				return &pullHit{repo: repo, pull: pull}
			}
		}
	}
	return nil
}

func nodeById(forge *Forge, id string) *gqlNode {
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	s := string(decoded)
	for _, kind := range []string{"Repository", "Issue", "PullRequest"} {
		if !strings.HasPrefix(s, "010:"+kind) {
			continue
		}
		dbID, err := strconv.Atoi(s[len("010:"+kind):])
		if err != nil {
			return nil
		}
		for _, repo := range forge.repos {
			if kind == "Repository" && repo.ID == dbID {
				n := repositoryNode(forge, repo)
				return &n
			}
			if kind == "Issue" {
				for _, issue := range repo.Issues {
					if issue.ID == dbID {
						n := issueNode(forge, repo, issue)
						return &n
					}
				}
			}
			if kind == "PullRequest" {
				for _, pull := range repo.Pulls {
					if pull.ID == dbID {
						n := pullNode(pull, repo, forge)
						return &n
					}
				}
			}
		}
	}
	return nil
}

func GraphQLHandler(forge *Forge, requestBody interface{}) (int, interface{}) {
	if list, ok := requestBody.([]interface{}); ok {
		var results []GraphQLResult
		for _, item := range list {
			if m, ok := item.(map[string]interface{}); ok {
				q, _ := m["query"].(string)
				vars, _ := m["variables"].(map[string]interface{})
				results = append(results, ExecuteGraphQL(forge, GraphQLRequest{Query: q, Variables: vars}))
			}
		}
		return 200, results
	}
	if m, ok := requestBody.(map[string]interface{}); ok {
		q, _ := m["query"].(string)
		vars, _ := m["variables"].(map[string]interface{})
		return 200, ExecuteGraphQL(forge, GraphQLRequest{Query: q, Variables: vars})
	}
	return 200, GraphQLResult{Data: nil, Errors: []GQLErrorEntry{{Message: "No operation"}}}
}
