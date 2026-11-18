// main.go: the whole entry — the env lifecycle (bind, register, wait for done) and the HTTP
// router across the four surfaces (git smart HTTP, REST v3, GraphQL v4, MCP).
// Runs as node /bot/main.ts spawning this vendored binary, or directly under a local driver.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var forge = NewForge()

var requestCounter int64

func nextRequestID() string {
	requestCounter++
	return fmt.Sprintf("req-%012x", requestCounter)
}

func writeJSON(w http.ResponseWriter, status int, body interface{}, headers map[string]string, withRequestID bool) {
	text, _ := json.Marshal(body)
	text = append(text, '\n')
	h := w.Header()
	h.Set("content-type", "application/json; charset=utf-8")
	h.Set("content-length", strconv.Itoa(len(text)))
	if withRequestID {
		h.Set("x-github-request-id", nextRequestID())
	}
	for k, v := range headers {
		h.Set(k, v)
	}
	w.WriteHeader(status)
	w.Write(text)
}

func readBody(r *http.Request) []byte {
	body, _ := io.ReadAll(r.Body)
	return body
}

var shopToken string

func authenticated(r *http.Request) bool {
	if shopToken == "" {
		return true
	}
	return r.Header.Get("authorization") == "Bearer "+shopToken
}

func hasAuthHeader(r *http.Request) bool {
	return r.Header.Get("authorization") != ""
}

func main() {
	shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")
	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	mux := http.NewServeMux()
	mux.HandleFunc("/", route)

	server := &http.Server{Handler: mux}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listener, err = net.Listen("tcp", ":0")
		if err != nil {
			os.Stderr.WriteString("[entry] bind failed: " + err.Error() + "\n")
			os.Exit(1)
		}
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			os.Stderr.WriteString("[entry] server error: " + err.Error() + "\n")
		}
	}()
	port := listener.Addr().(*net.TCPAddr).Port

	if envURL != "" {
		registerWithRetries(envURL, port)
		os.Stderr.WriteString(fmt.Sprintf("[entry] registered on port %d; waiting for done\n", port))
		waitForDone(envURL)
		server.Close()
		os.Stderr.WriteString("[entry] done\n")
		return
	}
	// no env (local smoke runs): serve until killed
	fmt.Printf("FORGE_PORT=%d\n", port)
	os.Stderr.WriteString(fmt.Sprintf("[entry] listening on port %d (no MAKEFUN_ENV_URL)\n", port))
	select {}
}

func registerWithRetries(envURL string, port int) {
	delay := 250
	for attempt := 0; ; attempt++ {
		client := &http.Client{Timeout: 30 * time.Second}
		body, _ := json.Marshal(map[string]interface{}{"port": port})
		req, err := http.NewRequest("POST", envURL+"/register", strings.NewReader(string(body)))
		if err == nil {
			req.Header.Set("content-type", "application/json")
			res, err := client.Do(req)
			if err == nil {
				if res.StatusCode == 200 || res.StatusCode == 409 {
					return
				}
				if res.StatusCode == 400 || res.StatusCode == 405 {
					respBody, _ := io.ReadAll(res.Body)
					os.Stderr.WriteString("[entry] register refused: HTTP " + strconv.Itoa(res.StatusCode) + " " + string(respBody) + "\n")
					os.Exit(1)
				}
			}
		}
		d := time.Duration(delay) * time.Millisecond
		if d > 2*time.Second {
			d = 2 * time.Second
		}
		time.Sleep(d)
		delay *= 2
	}
}

func waitForDone(envURL string) {
	delay := 250
	for attempt := 0; ; attempt++ {
		client := &http.Client{Timeout: 30 * time.Second}
		res, err := client.Get(envURL + "/done")
		if err == nil {
			var body struct {
				Done bool `json:"done"`
			}
			if json.NewDecoder(res.Body).Decode(&body) == nil && body.Done {
				return
			}
		}
		d := time.Duration(delay) * time.Millisecond
		if d > 2*time.Second {
			d = 2 * time.Second
		}
		time.Sleep(d)
		delay *= 2
	}
}

func route(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			writeJSON(w, 500, map[string]string{"message": "Internal error"}, nil, true)
		}
	}()
	path := r.URL.Path
	method := r.Method

	if method == "GET" && path == "/healthz" {
		writeJSON(w, 200, map[string]interface{}{"ok": true}, nil, true)
		return
	}

	// git smart HTTP
	if idx := strings.Index(path, ".git"); idx >= 0 {
		segments := strings.Split(strings.Trim(path, "/"), "/")
		if len(segments) >= 2 {
			owner := segments[0]
			repoName := segments[1]
			if suffix := strings.Index(repoName, ".git"); suffix >= 0 {
				repoName = repoName[:suffix]
			}
			rest := strings.Join(segments[2:], "/")
			protocolV2 := r.Header.Get("git-protocol") == "version=2"
			serviceParam := r.URL.Query().Get("service")
			if method == "GET" && (rest == "info/refs" || strings.HasSuffix(path, "/info/refs")) {
				service := serviceParam
				if service == "" {
					service = "git-upload-pack"
				}
				reply := InfoRefs(forge, owner, repoName, service, protocolV2 && service == "git-upload-pack")
				respondGit(w, reply)
				return
			}
			if method == "POST" && (rest == "git-upload-pack" || strings.HasSuffix(path, "/git-upload-pack")) {
				body := readBody(r)
				reply := safeUploadPack(forge, owner, repoName, body, protocolV2)
				respondGit(w, reply)
				return
			}
			if method == "POST" && (rest == "git-receive-pack" || strings.HasSuffix(path, "/git-receive-pack")) {
				body := readBody(r)
				reply := ReceivePack(forge, owner, repoName, body)
				respondGit(w, reply)
				return
			}
		}
		writeJSON(w, 404, map[string]string{"message": "Not Found"}, nil, true)
		return
	}

	// MCP
	if path == "/mcp" {
		body := readBody(r)
		var parsed interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			errBody, _ := json.Marshal(map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
			})
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(400)
			w.Write(errBody)
			return
		}
		answer := HandleMcp(forge, parsed, authenticated(r))
		headers := map[string]string{"mcp-session-id": nextRequestID()}
		writeJSON(w, answer.Status, answer.Body, headers, false)
		return
	}

	// GraphQL
	if path == "/api/graphql" {
		if shopToken != "" && !hasAuthHeader(r) {
			writeJSON(w, 401, map[string]string{"message": "Requires authentication"}, nil, true)
			return
		}
		if !authenticated(r) {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"}, nil, true)
			return
		}
		body := readBody(r)
		var parsed interface{}
		if err := json.Unmarshal(body, &parsed); err != nil {
			writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"}, nil, true)
			return
		}
		status, payload := GraphQLHandler(forge, parsed)
		writeJSON(w, status, payload, nil, true)
		return
	}

	// REST v3
	if path == "/api/v3" || path == "/api/v3/" || strings.HasPrefix(path, "/api/v3/") {
		if method == "GET" && (path == "/api/v3" || path == "/api/v3/") {
			if shopToken != "" && !hasAuthHeader(r) {
				writeJSON(w, 401, map[string]string{"message": "Requires authentication"}, nil, true)
				return
			}
			if !authenticated(r) {
				writeJSON(w, 401, map[string]string{"message": "Bad credentials"}, nil, true)
				return
			}
			writeJSON(w, 200, map[string]interface{}{
				"current_user_url": "/api/v3/user", "user_repositories_url": "/api/v3/user/repos",
				"repository_url": "/api/v3/repos/{owner}/{repo}", "meta_url": "/api/v3/meta",
			}, map[string]string{"x-oauth-scopes": "repo, read:org"}, true)
			return
		}
		if shopToken != "" && !hasAuthHeader(r) {
			writeJSON(w, 401, map[string]string{"message": "Requires authentication"}, nil, true)
			return
		}
		if !authenticated(r) {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"}, nil, true)
			return
		}
		var parsed interface{}
		if body := readBody(r); len(body) > 0 {
			if err := json.Unmarshal(body, &parsed); err != nil {
				writeJSON(w, 400, map[string]string{"message": "Problems parsing JSON"}, nil, true)
				return
			}
		}
		query := map[string]string{}
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				query[k] = v[0]
			}
		}
		origin := "http://" + r.Host
		reply := RestAPI(forge, method, path, parsed, query, origin)
		if reply.Status == 204 {
			w.Header().Set("x-github-request-id", nextRequestID())
			for k, v := range reply.Headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(204)
			return
		}
		writeJSON(w, reply.Status, reply.Body, reply.Headers, true)
		return
	}

	writeJSON(w, 404, map[string]string{"message": "Not Found"}, nil, true)
}

func safeUploadPack(forge *Forge, owner, repoName string, body []byte, protocolV2 bool) (reply GitReply) {
	defer func() {
		if rec := recover(); rec != nil {
			errBody, _ := json.Marshal(map[string]string{"message": "bad request"})
			reply = GitReply{Status: 400, ContentType: "application/json", Body: append(errBody, '\n')}
		}
	}()
	return UploadPack(forge, owner, repoName, body, protocolV2)
}

func respondGit(w http.ResponseWriter, reply GitReply) {
	if reply.Status != 200 {
		writeJSON(w, reply.Status, map[string]string{"message": "Not Found"}, nil, true)
		return
	}
	w.Header().Set("content-type", reply.ContentType)
	w.Header().Set("content-length", strconv.Itoa(len(reply.Body)))
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(reply.Status)
	w.Write(reply.Body)
}
