// rest.go: the GitHub REST v3 surface under /api/v3. Port of the entry's TS version.
package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type Reply struct {
	Status  int
	Headers map[string]string
	Body    interface{} // json-serializable, or []byte for empty 204
}

func jsonReply(status int, body interface{}, headers map[string]string) Reply {
	if headers == nil {
		headers = map[string]string{}
	}
	return Reply{Status: status, Headers: headers, Body: body}
}

func notFound() Reply { return jsonReply(404, map[string]string{"message": "Not Found"}, nil) }

func NodeID(kind string, id int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("010:%s%d", kind, id)))
}

func repoJSON(repo *Repo) map[string]interface{} {
	return map[string]interface{}{
		"id": repo.ID, "node_id": NodeID("Repository", repo.ID), "name": repo.Name,
		"full_name": repo.Owner + "/" + repo.Name, "private": repo.Private,
		"owner": map[string]interface{}{"login": repo.Owner, "id": ViewerID, "type": "User"},
		"description": repo.Description, "fork": false,
		"created_at": repo.CreatedAt, "updated_at": repo.UpdatedAt, "pushed_at": repo.UpdatedAt,
		"default_branch": repo.DefaultBranch, "has_issues": true, "has_pull_requests": true,
	}
}

func issueJSON(issue *Issue, repo *Repo) map[string]interface{} {
	var labels []interface{}
	for _, name := range issue.Labels {
		color := repo.Labels[name]
		if color == "" {
			color = "ededed"
		}
		labels = append(labels, map[string]interface{}{"name": name, "color": color})
	}
	return map[string]interface{}{
		"id": issue.ID, "node_id": NodeID("Issue", issue.ID), "number": issue.Number,
		"title": issue.Title, "body": issue.Body, "state": issue.State, "labels": labels,
		"user": map[string]interface{}{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"comments": len(issue.Comments), "created_at": issue.CreatedAt,
		"updated_at": issue.CreatedAt, "closed_at": issue.ClosedAt,
	}
}

func pullJSON2(pull *Pull, repo *Repo) map[string]interface{} {
	headSha := repo.Refs["refs/heads/"+pull.HeadRef]
	if headSha == "" {
		headSha = pull.HeadSha
	}
	baseSha := repo.Refs["refs/heads/"+pull.BaseRef]
	if baseSha == "" {
		baseSha = pull.BaseSha
	}
	return map[string]interface{}{
		"id": pull.ID, "node_id": NodeID("PullRequest", pull.ID), "number": pull.Number,
		"title": pull.Title, "body": pull.Body, "state": pull.State, "merged": pull.Merged,
		"merged_at": pull.MergedAt, "merge_commit_sha": pull.MergeCommitSha, "draft": false,
		"user": map[string]interface{}{"login": ViewerLogin, "id": ViewerID, "type": "User"},
		"head": map[string]interface{}{
			"ref": pull.HeadRef, "sha": headSha, "label": repo.Owner + ":" + pull.HeadRef,
			"repo": map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name},
		},
		"base": map[string]interface{}{
			"ref": pull.BaseRef, "sha": baseSha, "label": repo.Owner + ":" + pull.BaseRef,
			"repo": map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name},
		},
		"created_at": pull.CreatedAt, "updated_at": pull.CreatedAt,
		"mergeable": true,
	}
}

const repoNameChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

func badName(name string) bool {
	if len(name) == 0 || len(name) > 100 || strings.HasSuffix(name, ".git") {
		return true
	}
	for _, c := range name {
		if !strings.ContainsRune(repoNameChars, c) {
			return true
		}
	}
	return false
}

func RestAPI(forge *Forge, method, path string, body interface{}, query map[string]string, origin string) Reply {
	segments := splitPath(path)
	if len(segments) < 2 || segments[0] != "api" || segments[1] != "v3" {
		return notFound()
	}
	rest := segments[2:]

	// GET /user
	if rest[0] == "user" && len(rest) == 1 {
		return jsonReply(200, map[string]interface{}{
			"login": ViewerLogin, "id": ViewerID, "type": "User", "name": ViewerLogin,
			"email": ViewerLogin + "@example.invalid", "node_id": NodeID("User", ViewerID),
		}, nil)
	}
	if rest[0] == "user" && len(rest) >= 2 && rest[1] == "repos" {
		if method == "GET" {
			return listRepos(forge, query, origin)
		}
		if method == "POST" {
			b, _ := body.(map[string]interface{})
			name, _ := b["name"].(string)
			description, _ := b["description"].(string)
			isPrivate, _ := b["private"].(bool)
			if name == "" || badName(name) {
				return jsonReply(422, map[string]string{"message": "Repository creation failed. Validation failed: name is invalid."}, nil)
			}
			repo, err := forge.CreateRepo(name, description, isPrivate, ViewerLogin)
			if err != nil {
				return jsonReply(422, map[string]string{"message": "name already exists on this account"}, nil)
			}
			if autoInit, _ := b["auto_init"].(bool); autoInit {
				blob := forge.PutObject(repo, ObjBlob, []byte("# "+name+"\n"))
				files := map[string]FileEntry{"README.md": {OID: blob.OID, Mode: "100644", Type: "blob"}}
				oid := forge.CommitFiles(repo, files, "", "Initial commit", ViewerLogin)
				repo.Refs["refs/heads/"+repo.DefaultBranch] = oid
			}
			return jsonReply(201, repoJSON(repo), nil)
		}
		return notFound()
	}
	if rest[0] == "meta" {
		return jsonReply(200, map[string]string{"installed_version": "1.0.0", "version": "1.0.0"}, nil)
	}

	if rest[0] != "repos" || len(rest) < 3 {
		return notFound()
	}
	owner, name := rest[1], rest[2]
	repo := forge.GetRepo(owner, name)
	if repo == nil {
		return notFound()
	}
	restAfter := rest[3:]
	if len(restAfter) == 0 {
		switch method {
		case "GET":
			return jsonReply(200, repoJSON(repo), nil)
		case "PATCH":
			b, _ := body.(map[string]interface{})
			if d, ok := b["description"].(string); ok {
				repo.Description = d
			}
			repo.UpdatedAt = NowISO()
			return jsonReply(200, repoJSON(repo), nil)
		case "DELETE":
			delete(forge.repos, forge.repoKey(repo.Owner, repo.Name))
			return Reply{Status: 204, Headers: map[string]string{}, Body: []byte{}}
		}
		return notFound()
	}
	head := restAfter[0]
	tail := restAfter[1:]

	switch {
	case head == "branches" && len(tail) == 0 && method == "GET":
		var names []string
		for ref := range repo.Refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				names = append(names, ref[len("refs/heads/"):])
			}
		}
		sort.Strings(names)
		var out []interface{}
		for _, n := range names {
			out = append(out, map[string]interface{}{"name": n, "commit": map[string]interface{}{"sha": repo.Refs["refs/heads/"+n]}, "protected": false})
		}
		return jsonReply(200, out, nil)
	case head == "branches" && len(tail) == 1 && method == "GET":
		ref, ok := repo.Refs["refs/heads/"+tail[0]]
		if !ok {
			return jsonReply(404, map[string]string{"message": "Branch not found"}, nil)
		}
		return jsonReply(200, map[string]interface{}{"name": tail[0], "commit": map[string]interface{}{"sha": ref}, "protected": false}, nil)
	case head == "git" && len(tail) >= 1 && tail[0] == "refs" && method == "POST":
		b, _ := body.(map[string]interface{})
		ref, _ := b["ref"].(string)
		sha, _ := b["sha"].(string)
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			return jsonReply(422, map[string]string{"message": "Reference name is not well formed"}, nil)
		}
		if sha == "" {
			return jsonReply(422, map[string]string{"message": "Object does not exist"}, nil)
		}
		if _, ok := repo.Objects[sha]; !ok {
			return jsonReply(422, map[string]string{"message": "Object does not exist"}, nil)
		}
		if _, exists := repo.Refs[ref]; exists {
			return jsonReply(422, map[string]string{"message": "Reference already exists"}, nil)
		}
		repo.Refs[ref] = sha
		return jsonReply(201, map[string]interface{}{"ref": ref, "object": map[string]interface{}{"sha": sha, "type": "commit"}}, nil)
	case head == "git" && len(tail) == 2 && tail[0] == "refs" && method == "GET":
		full := tail[1]
		if !strings.HasPrefix(full, "refs/") {
			full = "refs/heads/" + full
		}
		ref, ok := repo.Refs[full]
		if !ok {
			return notFound()
		}
		return jsonReply(200, []interface{}{map[string]interface{}{"ref": full, "object": map[string]interface{}{"sha": ref, "type": "commit"}}}, nil)
	case head == "commits" && len(tail) == 0 && method == "GET":
		branch := query["sha"]
		if branch == "" {
			branch = repo.DefaultBranch
		}
		ref, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			if _, isOid := repo.Objects[branch]; isOid {
				ref = branch
			}
		}
		var out []interface{}
		if ref != "" && okOr(ref, repo) {
			seen := map[string]bool{}
			var queue []string
			queue = append(queue, ref)
			for len(queue) > 0 {
				oid := queue[0]
				queue = queue[1:]
				if seen[oid] {
					continue
				}
				seen[oid] = true
				view := forge.CommitView(repo, oid)
				if view == nil {
					continue
				}
				var parents []interface{}
				for _, p := range view.Parents {
					parents = append(parents, map[string]interface{}{"sha": p})
				}
				out = append(out, map[string]interface{}{
					"sha": oid,
					"commit": map[string]interface{}{
						"message": view.Message,
						"author":  map[string]interface{}{"name": view.AuthorName, "email": view.AuthorEmail},
						"committer": map[string]interface{}{"name": view.CommitterName, "email": view.CommitterEmail},
						"tree":      map[string]interface{}{"sha": view.Tree},
					},
					"author":    map[string]interface{}{"login": ViewerLogin},
					"committer": map[string]interface{}{"login": ViewerLogin},
					"parents":   parents,
				})
				queue = append(queue, view.Parents...)
			}
		}
		return jsonReply(200, out, nil)
	case head == "contents" && method == "PUT" && len(tail) >= 1:
		return putContents(forge, repo, joinPath(tail), body, query)
	case head == "contents" && method == "DELETE" && len(tail) >= 1:
		return deleteContents(forge, repo, joinPath(tail), body)
	case head == "contents" && method == "GET" && len(tail) >= 1:
		return getContents(forge, repo, joinPath(tail), query)
	case head == "labels" && method == "POST" && len(tail) == 0:
		b, _ := body.(map[string]interface{})
		name, _ := b["name"].(string)
		if name == "" {
			return jsonReply(422, map[string]string{"message": "Validation Failed"}, nil)
		}
		color, _ := b["color"].(string)
		if color == "" {
			color = "ededed"
		}
		if _, exists := repo.Labels[name]; exists {
			return jsonReply(422, map[string]string{"message": "Label already exists"}, nil)
		}
		repo.Labels[name] = color
		return jsonReply(201, map[string]interface{}{"name": name, "color": color}, nil)
	case head == "labels" && method == "GET" && len(tail) == 0:
		var out []interface{}
		for name, color := range repo.Labels {
			out = append(out, map[string]interface{}{"name": name, "color": color})
		}
		return jsonReply(200, out, nil)
	case head == "issues" && method == "POST" && len(tail) == 0:
		return createIssue(forge, repo, body)
	case head == "issues" && method == "GET" && len(tail) == 0:
		return listIssues(repo, query)
	case head == "issues" && len(tail) >= 1:
		return issueRoutes(forge, repo, method, tail, body)
	case head == "pulls" && method == "POST" && len(tail) == 0:
		return createPull(forge, repo, body)
	case head == "pulls" && method == "GET" && len(tail) == 0:
		return listPulls(repo, query)
	case head == "pulls" && len(tail) >= 1:
		return pullRoutes(forge, repo, method, tail, body)
	}
	return notFound()
}

func okOr(ref string, repo *Repo) bool {
	if _, ok := repo.Refs["refs/heads/"+ref]; ok {
		return true
	}
	_, ok := repo.Objects[ref]
	return ok
}

func joinPath(tail []string) string {
	return strings.Join(tail, "/")
}

func splitPath(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func listRepos(forge *Forge, query map[string]string, origin string) Reply {
	perPage := 30
	if v, err := strconv.Atoi(query["per_page"]); err == nil && v >= 1 && v <= 100 {
		perPage = v
	}
	page := 1
	if v, err := strconv.Atoi(query["page"]); err == nil && v >= 1 {
		page = v
	}
	var repos []*Repo
	for _, r := range forge.repos {
		if r.Owner == ViewerLogin {
			repos = append(repos, r)
		}
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].ID < repos[j].ID })
	start := (page - 1) * perPage
	var slice []*Repo
	if start < len(repos) {
		end := start + perPage
		if end > len(repos) {
			end = len(repos)
		}
		slice = repos[start:end]
	}
	var out []interface{}
	for _, r := range slice {
		out = append(out, repoJSON(r))
	}
	headers := map[string]string{}
	if start+perPage < len(repos) {
		headers["Link"] = fmt.Sprintf("<%s/api/v3/user/repos?per_page=%d&page=%d>; rel=\"next\"", origin, perPage, page+1)
	}
	return jsonReply(200, out, headers)
}

func putContents(forge *Forge, repo *Repo, path string, body interface{}, query map[string]string) Reply {
	b, _ := body.(map[string]interface{})
	message, _ := b["message"].(string)
	if message == "" {
		message = "update"
	}
	content := strings.ReplaceAll(strOr(b, "content"), "\n", "")
	branch := strOr(b, "branch")
	if branch == "" {
		branch = query["branch"]
	}
	if branch == "" {
		branch = query["ref"]
	}
	if branch == "" {
		branch = repo.DefaultBranch
	}
	bytes, err := base64.StdEncoding.DecodeString(content)
	if err != nil {
		return jsonReply(422, map[string]string{"message": "content is not valid base64"}, nil)
	}
	branchRef := "refs/heads/" + branch
	_, branchExists := repo.Refs[branchRef]
	if !branchExists && branch != repo.DefaultBranch {
		if head, ok := forge.HeadCommit(repo, ""); ok {
			repo.Refs[branchRef] = head
		}
	}
	baseTip := repo.Refs[branchRef]
	if baseTip == "" {
		baseTip, _ = forge.HeadCommit(repo, "")
	}
	baseFiles := map[string]FileEntry{}
	if baseTip != "" {
		if view := forge.CommitView(repo, baseTip); view != nil {
			baseFiles = forge.FilesAtTree(repo, view.Tree, "", nil)
		}
	}
	existing, hadExisting := baseFiles[path]
	isUpdate := hadExisting && existing.Type == "blob"
	if bsha, ok := b["sha"].(string); ok && bsha != "" {
		if !hadExisting || existing.OID != bsha {
			return jsonReply(409, map[string]string{"message": path + " does not match " + bsha}, nil)
		}
	}
	autoCreated := !branchExists && branch != repo.DefaultBranch
	blob := forge.PutObject(repo, ObjBlob, bytes)
	files := map[string]FileEntry{}
	for k, v := range baseFiles {
		files[k] = v
	}
	files[path] = FileEntry{OID: blob.OID, Mode: "100644", Type: "blob"}
	sha := forge.CommitFiles(repo, files, baseTip, message, ViewerLogin)
	repo.Refs[branchRef] = sha
	repo.UpdatedAt = NowISO()
	status := 201
	if isUpdate && !autoCreated {
		status = 200
	}
	return jsonReply(status, map[string]interface{}{
		"content": map[string]interface{}{
			"name": lastSegment(path), "path": path, "sha": blob.OID, "size": len(bytes),
			"encoding": "base64", "content": base64.StdEncoding.EncodeToString(bytes), "type": "file",
		},
		"commit": map[string]interface{}{"sha": sha, "message": message},
	}, nil)
}

func deleteContents(forge *Forge, repo *Repo, path string, body interface{}) Reply {
	b, _ := body.(map[string]interface{})
	message, _ := b["message"].(string)
	if message == "" {
		message = "delete"
	}
	branch := strOr(b, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	branchRef := "refs/heads/" + branch
	baseTip := repo.Refs[branchRef]
	if baseTip == "" {
		baseTip, _ = forge.HeadCommit(repo, "")
	}
	if baseTip == "" {
		return notFound()
	}
	files := forge.FilesAtTree(repo, forge.CommitView(repo, baseTip).Tree, "", nil)
	existing, ok := files[path]
	if !ok || existing.Type != "blob" {
		return notFound()
	}
	if bsha, has := b["sha"].(string); has && bsha != "" && existing.OID != bsha {
		return jsonReply(409, map[string]string{"message": path + " does not match " + bsha}, nil)
	}
	delete(files, path)
	sha := forge.CommitFiles(repo, files, baseTip, message, ViewerLogin)
	repo.Refs[branchRef] = sha
	return jsonReply(200, map[string]interface{}{"commit": map[string]interface{}{"sha": sha, "message": message}}, nil)
}

func getContents(forge *Forge, repo *Repo, path string, query map[string]string) Reply {
	refName := query["ref"]
	if refName == "" {
		refName = repo.DefaultBranch
	}
	tip := repo.Refs["refs/heads/"+refName]
	if tip == "" {
		tip, _ = forge.HeadCommit(repo, "")
	}
	if tip == "" {
		return notFound()
	}
	files := forge.FilesAtTree(repo, forge.CommitView(repo, tip).Tree, "", nil)
	entry, ok := files[path]
	if !ok {
		return notFound()
	}
	if entry.Type == "tree" {
		var out []interface{}
		var paths []string
		for p := range files {
			parent := ""
			if idx := strings.LastIndex(p, "/"); idx >= 0 {
				parent = p[:idx]
			}
			if parent == path {
				paths = append(paths, p)
			}
		}
		sort.Strings(paths)
		for _, p := range paths {
			e := files[p]
			blob, isBlob := repo.Objects[e.OID]
			size := 0
			if isBlob {
				size = len(blob.Data)
			}
			t := "file"
			if e.Type == "tree" {
				t = "dir"
			}
			out = append(out, map[string]interface{}{"type": t, "name": lastSegment(p), "path": p, "sha": e.OID, "size": size})
		}
		return jsonReply(200, out, nil)
	}
	blob := repo.Objects[entry.OID]
	return jsonReply(200, map[string]interface{}{
		"name": lastSegment(path), "path": path, "sha": entry.OID, "size": len(blob.Data),
		"encoding": "base64", "content": base64.StdEncoding.EncodeToString(blob.Data), "type": "file",
	}, nil)
}

func lastSegment(path string) string {
	if idx := strings.LastIndex(path, "/"); idx >= 0 {
		return path[idx+1:]
	}
	return path
}

func strOr(b map[string]interface{}, key string) string {
	if b == nil {
		return ""
	}
	s, _ := b[key].(string)
	return s
}

func createIssue(forge *Forge, repo *Repo, body interface{}) Reply {
	b, _ := body.(map[string]interface{})
	title, _ := b["title"].(string)
	if title == "" {
		return jsonReply(422, map[string]string{"message": "Validation Failed: title is required"}, nil)
	}
	text, _ := b["body"].(string)
	var labels []string
	if raw, ok := b["labels"].([]interface{}); ok {
		for _, l := range raw {
			if s, ok := l.(string); ok {
				labels = append(labels, s)
			}
		}
	}
	for _, label := range labels {
		if _, exists := repo.Labels[label]; !exists {
			return jsonReply(422, map[string]string{"message": fmt.Sprintf("Validation Failed: label \"%s\" does not exist", label)}, nil)
		}
	}
	number := repo.NextIssueNumber
	repo.NextIssueNumber++
	issue := &Issue{ID: 9000 + number, Number: number, Title: title, Body: text, State: "open", Labels: labels, CreatedAt: NowISO()}
	repo.Issues[number] = issue
	_ = forge
	return jsonReply(201, issueJSON(issue, repo), nil)
}

func listIssues(repo *Repo, query map[string]string) Reply {
	state := queryOr(query, "state", "open")
	sortKey := queryOr(query, "sort", "created")
	direction := queryOr(query, "direction", "desc")
	var issues []*Issue
	for _, i := range repo.Issues {
		if i.IsPull {
			continue
		}
		if state != "all" && i.State != state {
			continue
		}
		issues = append(issues, i)
	}
	sort.Slice(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		var less bool
		switch sortKey {
		case "created":
			less = a.Number < b.Number
		case "updated":
			less = a.CreatedAt < b.CreatedAt
		case "comments":
			less = len(a.Comments) < len(b.Comments)
		default:
			less = a.Number < b.Number
		}
		if direction == "desc" {
			return !less
		}
		return less
	})
	var out []interface{}
	for _, i := range issues {
		out = append(out, issueJSON(i, repo))
	}
	return jsonReply(200, out, nil)
}

func issueRoutes(forge *Forge, repo *Repo, method string, tail []string, body interface{}) Reply {
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		return notFound()
	}
	issue, ok := repo.Issues[num]
	if !ok {
		return notFound()
	}
	if len(tail) == 1 {
		if method == "GET" {
			return jsonReply(200, issueJSON(issue, repo), nil)
		}
		if method == "PATCH" {
			b, _ := body.(map[string]interface{})
			if s, ok := b["state"].(string); ok {
				if s == "closed" {
					issue.State = "closed"
					issue.ClosedAt = NowISO()
				} else if s == "open" {
					issue.State = "open"
					issue.ClosedAt = ""
				}
			}
			if t, ok := b["title"].(string); ok {
				issue.Title = t
			}
			if t, ok := b["body"].(string); ok {
				issue.Body = t
			}
			return jsonReply(200, issueJSON(issue, repo), nil)
		}
		return notFound()
	}
	if tail[1] == "comments" {
		if method == "POST" {
			b, _ := body.(map[string]interface{})
			text, _ := b["body"].(string)
			comment := Comment{ID: repo.NextCommentID, Body: text, Login: ViewerLogin, CreatedAt: NowISO()}
			repo.NextCommentID++
			issue.Comments = append(issue.Comments, comment)
			return jsonReply(201, map[string]interface{}{
				"id": comment.ID, "body": comment.Body,
				"user": map[string]interface{}{"login": ViewerLogin}, "created_at": comment.CreatedAt,
			}, nil)
		}
		if method == "GET" {
			var out []interface{}
			for _, c := range issue.Comments {
				out = append(out, map[string]interface{}{
					"id": c.ID, "body": c.Body,
					"user": map[string]interface{}{"login": c.Login}, "created_at": c.CreatedAt,
				})
			}
			return jsonReply(200, out, nil)
		}
	}
	return notFound()
}

func createPull(forge *Forge, repo *Repo, body interface{}) Reply {
	b, _ := body.(map[string]interface{})
	title, _ := b["title"].(string)
	headRef, _ := b["head"].(string)
	baseRef, _ := b["base"].(string)
	text, _ := b["body"].(string)
	if title == "" || headRef == "" || baseRef == "" {
		return jsonReply(422, map[string]string{"message": "Validation Failed"}, nil)
	}
	headSha, hasHead := repo.Refs["refs/heads/"+headRef]
	if !hasHead {
		return jsonReply(422, map[string]string{"message": "Validation Failed: head " + headRef + " does not exist"}, nil)
	}
	baseSha, hasBase := repo.Refs["refs/heads/"+baseRef]
	if !hasBase {
		return jsonReply(422, map[string]string{"message": "Validation Failed: base " + baseRef + " does not exist"}, nil)
	}
	number := repo.NextIssueNumber
	repo.NextIssueNumber++
	pull := &Pull{ID: 9500 + number, Number: number, Title: title, Body: text, State: "open",
		HeadRef: headRef, HeadSha: headSha, BaseRef: baseRef, BaseSha: baseSha,
		MergedBy: ViewerLogin, CreatedAt: NowISO()}
	repo.Pulls[number] = pull
	_ = forge
	return jsonReply(201, pullJSON2(pull, repo), nil)
}

func listPulls(repo *Repo, query map[string]string) Reply {
	state := queryOr(query, "state", "open")
	var pulls []*Pull
	for _, p := range repo.Pulls {
		switch state {
		case "open":
			if p.State == "open" && !p.Merged {
				pulls = append(pulls, p)
			}
		case "closed":
			if p.State == "closed" {
				pulls = append(pulls, p)
			}
		default:
			pulls = append(pulls, p)
		}
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
	var out []interface{}
	for _, p := range pulls {
		out = append(out, pullJSON2(p, repo))
	}
	return jsonReply(200, out, nil)
}

func pullRoutes(forge *Forge, repo *Repo, method string, tail []string, body interface{}) Reply {
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		return notFound()
	}
	pull, ok := repo.Pulls[num]
	if !ok {
		return notFound()
	}
	if len(tail) == 1 && method == "GET" {
		return jsonReply(200, pullJSON2(pull, repo), nil)
	}
	if tail[1] == "files" && method == "GET" {
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
			return jsonReply(200, []interface{}{}, nil)
		}
		rows := forge.DiffTrees(repo, forge.FilesAtTree(repo, baseView.Tree, "", nil), forge.FilesAtTree(repo, headView.Tree, "", nil))
		var out []interface{}
		for _, row := range rows {
			out = append(out, map[string]interface{}{
				"filename": row.Filename, "status": row.Status,
				"additions": row.Additions, "deletions": row.Deletions, "changes": row.Changes,
			})
		}
		return jsonReply(200, out, nil)
	}
	if tail[1] == "merge" && method == "PUT" {
		result := forge.MergePull(repo, pull, "squash")
		if !result.OK {
			return jsonReply(result.Status, map[string]string{"message": result.Message}, nil)
		}
		return jsonReply(200, map[string]interface{}{"sha": result.SHA, "merged": true, "message": result.Message}, nil)
	}
	return notFound()
}

func queryOr(query map[string]string, key, fallback string) string {
	if v, ok := query[key]; ok && v != "" {
		return v
	}
	return fallback
}
