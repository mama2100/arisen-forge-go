// mcp.go: the Model Context Protocol endpoint at /mcp. Port of the entry's TS version.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type ToolDef struct {
	Name        string
	Description string
	Props       []ToolProp
}

type ToolProp struct {
	Name        string
	Type        string
	Description string
	Required    bool
}

var tools = []ToolDef{
	{"create_repo", "Create a new repository owned by the authenticated user", []ToolProp{
		{"name", "string", "The repository name", true},
		{"description", "string", "The repository description", false},
		{"private", "boolean", "Whether the repository is private", false},
	}},
	{"get_repo", "Get one repository", []ToolProp{
		{"owner", "string", "The repository owner login", true},
		{"repo", "string", "The repository name", true},
	}},
	{"create_issue", "Create an issue in a repository", []ToolProp{
		{"owner", "string", "The repository owner login", true},
		{"repo", "string", "The repository name", true},
		{"title", "string", "The issue title", true},
		{"body", "string", "The issue body", false},
	}},
	{"list_issues", "List issues in a repository", []ToolProp{
		{"owner", "string", "The repository owner login", true},
		{"repo", "string", "The repository name", true},
		{"state", "string", "open, closed or all", false},
	}},
	{"create_pull_request", "Open a pull request from a head branch to a base branch", []ToolProp{
		{"owner", "string", "The repository owner login", true},
		{"repo", "string", "The repository name", true},
		{"title", "string", "The pull request title", true},
		{"head", "string", "The head branch name", true},
		{"base", "string", "The base branch name", true},
	}},
	{"merge_pull_request", "Merge a pull request (squash)", []ToolProp{
		{"owner", "string", "The repository owner login", true},
		{"repo", "string", "The repository name", true},
		{"pull_number", "number", "The pull request number", true},
	}},
}

func toolsListResult() map[string]interface{} {
	toolList := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		props := map[string]interface{}{}
		var required []string
		for _, p := range tool.Props {
			props[p.Name] = map[string]interface{}{"type": p.Type, "description": p.Description}
			if p.Required {
				required = append(required, p.Name)
			}
		}
		toolList = append(toolList, map[string]interface{}{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": map[string]interface{}{"type": "object", "properties": props, "required": required},
		})
	}
	return map[string]interface{}{"tools": toolList}
}

func normalize(text string) string {
	var b strings.Builder
	for _, c := range text {
		if c >= 'A' && c <= 'Z' {
			b.WriteRune(c + 32)
		} else if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func toolByInvokedName(name string) *ToolDef {
	bare := strings.TrimPrefix(normalize(name), "github_")
	for i := range tools {
		tn := normalize(tools[i].Name)
		if strings.HasSuffix(tn, bare) || strings.HasSuffix(bare, tn) {
			return &tools[i]
		}
	}
	return nil
}

func mapArguments(tool *ToolDef, rawArgs map[string]interface{}, extras map[string]interface{}) map[string]interface{} {
	mapped := map[string]interface{}{}
	for k, v := range extras {
		mapped[k] = v
	}
	for _, prop := range tool.Props {
		if v, ok := rawArgs[prop.Name]; ok {
			mapped[prop.Name] = v
			continue
		}
		for k, v := range rawArgs {
			if normalize(k) == normalize(prop.Name) {
				mapped[prop.Name] = v
			}
		}
	}
	return mapped
}

func rpcError(id interface{}, code int, message string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]interface{}{"code": code, "message": message},
	}
}

func rpcResult(id interface{}, result interface{}) map[string]interface{} {
	return map[string]interface{}{"jsonrpc": "2.0", "id": id, "result": result}
}

func toolCallResult(value interface{}, isError bool) map[string]interface{} {
	text := ""
	if s, ok := value.(string); ok {
		text = s
	} else {
		b, _ := json.Marshal(value)
		text = string(b)
	}
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": text}},
		"isError": isError,
	}
}

func runTool(forge *Forge, tool *ToolDef, rawArgs map[string]interface{}, extras map[string]interface{}) (bool, interface{}) {
	args := mapArguments(tool, rawArgs, extras)
	for _, prop := range tool.Props {
		if prop.Required {
			if _, ok := args[prop.Name]; !ok {
				return false, fmt.Sprintf("missing required argument %s", prop.Name)
			}
		}
	}
	switch tool.Name {
	case "create_repo":
		name, _ := args["name"].(string)
		description, _ := args["description"].(string)
		private, _ := args["private"].(bool)
		repo, err := forge.CreateRepo(name, description, private, ViewerLogin)
		if err != nil {
			return false, "repository creation failed"
		}
		return true, map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name, "private": repo.Private, "default_branch": repo.DefaultBranch}
	case "get_repo":
		repo := forge.GetRepo(strOf(args["owner"]), strOf(args["repo"]))
		if repo == nil {
			return false, "repository not found"
		}
		return true, map[string]interface{}{"id": repo.ID, "name": repo.Name, "full_name": repo.Owner + "/" + repo.Name, "private": repo.Private, "default_branch": repo.DefaultBranch, "description": repo.Description}
	case "create_issue":
		repo := forge.GetRepo(strOf(args["owner"]), strOf(args["repo"]))
		if repo == nil {
			return false, "repository not found"
		}
		title := strOf(args["title"])
		if title == "" {
			return false, "missing required argument title"
		}
		number := repo.NextIssueNumber
		repo.NextIssueNumber++
		issue := &Issue{ID: 9000 + number, Number: number, Title: title, Body: strOf(args["body"]), State: "open", CreatedAt: NowISO()}
		repo.Issues[number] = issue
		return true, map[string]interface{}{"number": issue.Number, "id": issue.ID, "title": issue.Title, "state": "open", "body": issue.Body}
	case "list_issues":
		repo := forge.GetRepo(strOf(args["owner"]), strOf(args["repo"]))
		if repo == nil {
			return false, "repository not found"
		}
		state := strOf(args["state"])
		if state == "" {
			state = "open"
		}
		var out []interface{}
		nums := []int{}
		for n := range repo.Issues {
			nums = append(nums, n)
		}
		sortInts(nums)
		for _, n := range nums {
			issue := repo.Issues[n]
			if issue.IsPull || (state != "all" && issue.State != state) {
				continue
			}
			out = append(out, map[string]interface{}{"number": issue.Number, "id": issue.ID, "title": issue.Title, "state": issue.State, "body": issue.Body})
		}
		return true, out
	case "create_pull_request":
		repo := forge.GetRepo(strOf(args["owner"]), strOf(args["repo"]))
		if repo == nil {
			return false, "repository not found"
		}
		headRef := strOf(args["head"])
		baseRef := strOf(args["base"])
		title := strOf(args["title"])
		if title == "" || headRef == "" || baseRef == "" {
			return false, "missing required arguments"
		}
		headSha := repo.Refs["refs/heads/"+headRef]
		baseSha := repo.Refs["refs/heads/"+baseRef]
		if headSha == "" || baseSha == "" {
			return false, "branch not found"
		}
		number := repo.NextIssueNumber
		repo.NextIssueNumber++
		pull := &Pull{ID: 9500 + number, Number: number, Title: title, State: "open", HeadRef: headRef, HeadSha: headSha, BaseRef: baseRef, BaseSha: baseSha, MergedBy: ViewerLogin, CreatedAt: NowISO()}
		repo.Pulls[number] = pull
		return true, map[string]interface{}{"number": pull.Number, "id": pull.ID, "title": pull.Title, "state": "open", "head": pull.HeadRef, "base": pull.BaseRef}
	case "merge_pull_request":
		repo := forge.GetRepo(strOf(args["owner"]), strOf(args["repo"]))
		if repo == nil {
			return false, "repository not found"
		}
		num := intOf(args["pull_number"])
		pull, ok := repo.Pulls[num]
		if !ok {
			return false, "pull request not found"
		}
		result := forge.MergePull(repo, pull, "squash")
		if !result.OK {
			return false, result.Message
		}
		return true, map[string]interface{}{"merged": true, "sha": result.SHA, "message": result.Message, "number": pull.Number}
	}
	return false, "unknown tool"
}

func strOf(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func intOf(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := fmt.Sscanf(x, "%d")
		return n
	}
	return 0
}

func sortInts(list []int) {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j] < list[i] {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
}

type McpAnswer struct {
	Status int
	Body   interface{} // map or []byte
}

// HandleMcp processes one JSON-RPC request body (object) or a batch array.
func HandleMcp(forge *Forge, requestBody interface{}, authenticated bool) McpAnswer {
	if requestBody == nil {
		return McpAnswer{400, rpcError(nil, -32700, "Parse error")}
	}
	if list, isBatch := requestBody.([]interface{}); isBatch {
		var out []interface{}
		for _, item := range list {
			answer := handleOne(forge, item, authenticated)
			if answer.Stop {
				return McpAnswer{answer.Status, answer.Value}
			}
			if answer.Value != nil {
				out = append(out, answer.Value)
			}
		}
		if len(out) == 1 {
			return McpAnswer{200, out[0]}
		}
		return McpAnswer{200, out}
	}
	answer := handleOne(forge, requestBody, authenticated)
	if answer.Stop {
		return McpAnswer{answer.Status, answer.Value}
	}
	if answer.Value == nil {
		return McpAnswer{202, nil}
	}
	return McpAnswer{200, answer.Value}
}

type oneAnswer struct {
	Status int
	Value  interface{}
	Stop   bool
}

func handleOne(forge *Forge, item interface{}, authenticated bool) oneAnswer {
	call, ok := item.(map[string]interface{})
	if !ok {
		return oneAnswer{Value: rpcError(nil, -32700, "Parse error")}
	}
	method, _ := call["method"].(string)
	id := call["id"]
	_, isNotification := call["id"]
	if !isNotification {
		id = nil
	}
	isNotification = call["id"] == nil
	if !authenticated {
		return oneAnswer{Status: 401, Value: rpcError(id, -32001, "Unauthorized"), Stop: true}
	}
	switch method {
	case "initialize":
		params, _ := call["params"].(map[string]interface{})
		requested, _ := params["protocolVersion"].(string)
		version := requested
		if !containsStr(supportedVersions, requested) {
			version = supportedVersions[0]
		}
		return oneAnswer{Value: rpcResult(id, map[string]interface{}{
			"protocolVersion": version,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
			"serverInfo":      map[string]interface{}{"name": "github-compatible-forge", "version": "1.0.0"},
		})}
	case "notifications/initialized":
		if isNotification {
			return oneAnswer{Status: 202, Stop: true}
		}
		return oneAnswer{Value: rpcResult(id, map[string]interface{}{})}
	case "server/discover":
		return oneAnswer{Value: rpcResult(id, map[string]interface{}{
			"supportedVersions": supportedVersions,
			"serverInfo":        map[string]interface{}{"name": "github-compatible-forge", "version": "1.0.0"},
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{"listChanged": false}},
		})}
	case "tools/list":
		return oneAnswer{Value: rpcResult(id, toolsListResult())}
	case "tools/call":
		params, _ := call["params"].(map[string]interface{})
		name, _ := params["name"].(string)
		tool := toolByInvokedName(name)
		if tool == nil {
			return oneAnswer{Value: rpcError(id, -32602, fmt.Sprintf("Unknown tool: %s", name))}
		}
		rawArgs, _ := params["arguments"].(map[string]interface{})
		if rawArgs == nil {
			rawArgs = map[string]interface{}{}
		}
		extras := map[string]interface{}{}
		if fullName, ok := rawArgs["full_name"].(string); ok && strings.Contains(fullName, "/") {
			parts := strings.SplitN(fullName, "/", 2)
			if _, ok := rawArgs["owner"]; !ok {
				extras["owner"] = parts[0]
			}
			if _, ok := rawArgs["repo"]; !ok {
				extras["repo"] = parts[1]
			}
		}
		ran, value := runTool(forge, tool, rawArgs, extras)
		return oneAnswer{Value: rpcResult(id, toolCallResult(value, !ran))}
	case "ping":
		return oneAnswer{Value: rpcResult(id, map[string]interface{}{})}
	}
	if isNotification {
		return oneAnswer{}
	}
	return oneAnswer{Value: rpcError(id, -32601, fmt.Sprintf("Method not found: %s", method))}
}
