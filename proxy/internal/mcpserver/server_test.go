package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var devIdentity = Identity{Username: "dev-editor", Name: "Dev Editor", Email: "dev@prodeko.org"}

// quiet keeps the per-call audit lines out of the test output; what they say is
// asserted nowhere, and a failing test reads better without them.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testTool(name string) Tool {
	return Tool{
		Name:        name,
		Description: "a tool",
		Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Call: func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
			return Text("ok"), nil
		},
	}
}

// textOf reads a text block. The member is a pointer on the wire, so a block
// that carries no text at all is told apart from one that carries "".
func textOf(t *testing.T, c content) string {
	t.Helper()
	if c.Text == nil {
		t.Fatalf("content block %+v has no text member", c)
	}
	return *c.Text
}

func testServer(t *testing.T, tools ...Tool) *Server {
	t.Helper()
	s, err := New(Config{
		Name:                "prodeko-content-editor",
		Version:             "test",
		Tools:               tools,
		Authenticate:        StaticBearer("secret", devIdentity),
		ResourceMetadataURL: "https://mcp.prodeko.org/.well-known/oauth-protected-resource",
		Logger:              quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// An unauthenticated /mcp would be a write credential on the open internet.
func TestNewRequiresAnAuthenticator(t *testing.T) {
	_, err := New(Config{Name: "x", Tools: []Tool{testTool("a")}})
	if err == nil {
		t.Fatal("New accepted a configuration with no Authenticator")
	}
}

func TestNewRejectsDuplicateAndUnusableTools(t *testing.T) {
	auth := StaticBearer("secret", devIdentity)

	if _, err := New(Config{Name: "x", Authenticate: auth, Tools: []Tool{testTool("a"), testTool("a")}}); err == nil {
		t.Error("New accepted two tools with the same name")
	}

	bad := testTool("a")
	bad.Schema = json.RawMessage(`{"type":`)
	if _, err := New(Config{Name: "x", Authenticate: auth, Tools: []Tool{bad}}); err == nil {
		t.Error("New accepted a tool whose schema is not JSON")
	}

	noCall := testTool("a")
	noCall.Call = nil
	if _, err := New(Config{Name: "x", Authenticate: auth, Tools: []Tool{noCall}}); err == nil {
		t.Error("New accepted a tool with no Call")
	}
}

func TestToolsKeepRegistrationOrder(t *testing.T) {
	s := testServer(t, testTool("a"), testTool("b"), testTool("c"))
	var got []string
	for _, tool := range s.Tools() {
		got = append(got, tool.Name)
	}
	if want := "a,b,c"; strings.Join(got, ",") != want {
		t.Fatalf("Tools() order = %v, want %s", got, want)
	}
}

func TestMissingBearerIsRefusedWithDiscovery(t *testing.T) {
	s := testServer(t, testTool("a"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, Path, strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	challenge := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(challenge, "Bearer ") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", challenge)
	}
	if !strings.Contains(challenge, `resource_metadata="https://mcp.prodeko.org/.well-known/oauth-protected-resource"`) {
		t.Fatalf("WWW-Authenticate = %q, want it to point at the resource metadata", challenge)
	}
}

func TestWrongBearerIsRefused(t *testing.T) {
	s := testServer(t, testTool("a"))
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// GET is not an SSE channel here, and saying so beats a client hanging on one.
func TestGetIsMethodNotAllowed(t *testing.T) {
	s := testServer(t, testTool("a"))
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("Allow = %q, want POST", got)
	}
}

func TestBearerFromHeader(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Bearer abc", "abc"},
		{"bearer abc", "abc"},
		{"BEARER  abc  ", "abc"},
		{"Basic abc", ""},
		{"abc", ""},
		{"", ""},
	} {
		if got := BearerFromHeader(tc.in); got != tc.want {
			t.Errorf("BearerFromHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstOfAcceptsTheFirstMatch(t *testing.T) {
	other := Identity{Username: "maija"}
	auth := FirstOf(StaticBearer("", devIdentity), StaticBearer("two", other))

	if _, err := auth("nope"); err == nil {
		t.Error("FirstOf accepted an unknown token")
	}
	id, err := auth("two")
	if err != nil {
		t.Fatalf("FirstOf: %v", err)
	}
	if id.Username != "maija" {
		t.Fatalf("identity = %+v, want maija", id)
	}
}

// post sends body to the transport as the authenticated dev editor.
func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// wire is a response decoded loosely, so a test can assert on what a client
// would actually read rather than on this package's own types.
type wire struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	} `json:"error"`
}

func decodeOne(t *testing.T, rec *httptest.ResponseRecorder) wire {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	var w wire
	if err := json.Unmarshal(rec.Body.Bytes(), &w); err != nil {
		t.Fatalf("response %s: %v", rec.Body.String(), err)
	}
	if w.JSONRPC != "2.0" {
		t.Fatalf("jsonrpc = %q, want 2.0", w.JSONRPC)
	}
	if (w.Result == nil) == (w.Error == nil) {
		t.Fatalf("response %s carries neither a result nor an error, or both", rec.Body.String())
	}
	return w
}

func TestInitializeHandshake(t *testing.T) {
	s, err := New(Config{
		Name:         "prodeko-content-editor",
		Version:      "1.2.3",
		Instructions: "two content roots, tokens over hex",
		Tools:        []Tool{testTool("a")},
		Authenticate: StaticBearer("secret", devIdentity),
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"claude.ai","version":"1"},"capabilities":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeOne(t, rec)

	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	// One version is spoken; a client asking for another is answered with ours.
	if res.ProtocolVersion != ProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", res.ProtocolVersion, ProtocolVersion)
	}
	if res.Capabilities.Tools == nil {
		t.Error("capabilities do not advertise tools")
	}
	if res.ServerInfo.Name != "prodeko-content-editor" || res.ServerInfo.Version != "1.2.3" {
		t.Errorf("serverInfo = %+v", res.ServerInfo)
	}
	if res.Instructions != "two content roots, tokens over hex" {
		t.Errorf("instructions = %q, want them passed through", res.Instructions)
	}
	if string(got.ID) != "1" {
		t.Errorf("id = %s, want 1", got.ID)
	}
}

// The handshake ends with a notification, which has no reply at all.
func TestInitializedNotificationIsAccepted(t *testing.T) {
	s := testServer(t, testTool("a"))
	rec := post(t, s, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want none", rec.Body.String())
	}
}

func TestPingAnswersAnEmptyResult(t *testing.T) {
	s := testServer(t, testTool("a"))
	got := decodeOne(t, post(t, s, `{"jsonrpc":"2.0","id":"p","method":"ping"}`))

	if string(got.Result) != "{}" {
		t.Fatalf("result = %s, want {}", got.Result)
	}
	if string(got.ID) != `"p"` {
		t.Fatalf("id = %s, want the string id echoed back", got.ID)
	}
}

func TestToolsListShape(t *testing.T) {
	read := testTool("read_file")
	read.Description = "Read a file"
	read.Schema = json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)
	s := testServer(t, testTool("get_conventions"), read)

	got := decodeOne(t, post(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("tools/list result: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(res.Tools))
	}
	if res.Tools[0].Name != "get_conventions" || res.Tools[1].Name != "read_file" {
		t.Errorf("tools are not in registration order: %s, %s", res.Tools[0].Name, res.Tools[1].Name)
	}
	if res.Tools[1].Description != "Read a file" {
		t.Errorf("description = %q", res.Tools[1].Description)
	}
	// The schema must arrive as the object the tool declared, not as a string.
	var schema map[string]any
	if err := json.Unmarshal(res.Tools[1].InputSchema, &schema); err != nil {
		t.Fatalf("inputSchema is not an object: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("inputSchema type = %v, want object", schema["type"])
	}
}

// A tool sees the arguments and the identity the bearer token proved.
func TestCallToolDispatch(t *testing.T) {
	var (
		gotArgs string
		gotUser Identity
	)
	echo := Tool{
		Name:   "read_file",
		Schema: json.RawMessage(`{"type":"object"}`),
		Call: func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
			gotArgs, gotUser = string(args), id
			return Text("# Tapahtumat\n"), nil
		},
	}
	s := testServer(t, echo)

	got := decodeOne(t, post(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"site/content/fi/_index.md"}}}`))
	if gotArgs != `{"path":"site/content/fi/_index.md"}` {
		t.Errorf("arguments = %s", gotArgs)
	}
	if gotUser != devIdentity {
		t.Errorf("identity = %+v, want %+v", gotUser, devIdentity)
	}

	var res callToolResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("tools/call result: %v", err)
	}
	if res.IsError {
		t.Error("a tool that succeeded was reported as an error")
	}
	if len(res.Content) != 1 || res.Content[0].Type != "text" || textOf(t, res.Content[0]) != "# Tapahtumat\n" {
		t.Fatalf("content = %+v", res.Content)
	}
}

// A screenshot is only worth taking if the model gets to look at it: the PNG
// travels as an MCP image block beside the prose, base64 and typed, and the
// prose comes first because clients render the blocks in order.
func TestCallToolReturnsImagesAfterTheProse(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01}
	shot := testTool("screenshot")
	shot.Call = func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
		return Result{Text: "Captured /fi/ at 1280 px.", Images: []Image{{PNG: png}}}, nil
	}
	s := testServer(t, shot)

	got := decodeOne(t, post(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"screenshot","arguments":{}}}`))
	var res callToolResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("tools/call result: %v", err)
	}
	if len(res.Content) != 2 {
		t.Fatalf("content blocks = %d, want the prose and the picture", len(res.Content))
	}
	if res.Content[0].Type != "text" || textOf(t, res.Content[0]) != "Captured /fi/ at 1280 px." {
		t.Errorf("first block = %+v, want the prose", res.Content[0])
	}
	img := res.Content[1]
	if img.Type != "image" || img.MIMEType != "image/png" {
		t.Errorf("second block = %+v, want a PNG image block", img)
	}
	if img.Text != nil {
		t.Errorf("an image block carries a text member: %+v", img)
	}
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil {
		t.Fatalf("image data is not base64: %v", err)
	}
	if !bytes.Equal(data, png) {
		t.Errorf("image data = %x, want the PNG the tool returned", data)
	}
}

// A tool that takes no arguments is called with an empty object whether or not
// the client sent one.
func TestCallToolFillsInAbsentArguments(t *testing.T) {
	for _, params := range []string{
		`{"name":"a"}`,
		`{"name":"a","arguments":null}`,
		`{"name":"a","arguments":{}}`,
	} {
		var got string
		tool := testTool("a")
		tool.Call = func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
			got = string(args)
			return Text("ok"), nil
		}
		s := testServer(t, tool)

		post(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`)
		if got != "{}" {
			t.Errorf("params %s gave the tool %q, want {}", params, got)
		}
	}
}

// A refusal is a result the model reads, not a transport error. The fence
// saying no is the ordinary case of this.
func TestToolErrorBecomesAnIsErrorResult(t *testing.T) {
	failing := testTool("write_file")
	failing.Call = func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
		return Result{}, errors.New("fence: site/layouts is read-only")
	}
	s := testServer(t, failing)

	rec := post(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"write_file","arguments":{}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: a tool refusal is not a transport failure", rec.Code)
	}
	got := decodeOne(t, rec)
	if got.Error != nil {
		t.Fatalf("a tool refusal was reported as a JSON-RPC error: %+v", got.Error)
	}
	var res callToolResult
	if err := json.Unmarshal(got.Result, &res); err != nil {
		t.Fatalf("tools/call result: %v", err)
	}
	if !res.IsError {
		t.Error("isError is not set on a failed tool call")
	}
	if len(res.Content) != 1 || !strings.Contains(textOf(t, res.Content[0]), "read-only") {
		t.Fatalf("content = %+v, want the rule that was broken", res.Content)
	}
}

// A panicking tool must not cost the connection or the rest of a batch.
func TestPanickingToolIsContained(t *testing.T) {
	boom := testTool("build")
	boom.Call = func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
		panic("nil map")
	}
	s := testServer(t, boom, testTool("ping_tool"))

	rec := post(t, s, `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"build"}},{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping_tool"}}]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var batch []wire
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("answers = %d, want 2: the panic swallowed the rest of the batch", len(batch))
	}
	var res callToolResult
	if err := json.Unmarshal(batch[0].Result, &res); err != nil {
		t.Fatalf("result: %v", err)
	}
	if !res.IsError {
		t.Error("a panicking tool was reported as a success")
	}
}

func TestProtocolErrors(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   int
		wantIn     string // a substring of the message or the data
	}{
		{
			name:       "not json",
			body:       `{"jsonrpc":"2.0",`,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeParseError,
		},
		{
			name:       "empty body",
			body:       "   ",
			wantStatus: http.StatusBadRequest,
			wantCode:   codeParseError,
		},
		{
			name:       "empty batch",
			body:       `[]`,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalidRequest,
		},
		{
			name:       "wrong jsonrpc version",
			body:       `{"jsonrpc":"1.0","id":1,"method":"ping"}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidRequest,
			wantIn:     "2.0",
		},
		{
			name:       "null id",
			body:       `{"jsonrpc":"2.0","id":null,"method":"ping"}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidRequest,
			wantIn:     "must not be null",
		},
		{
			name:       "no method",
			body:       `{"jsonrpc":"2.0","id":1,"result":{}}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidRequest,
			wantIn:     "no method",
		},
		{
			name:       "unknown method",
			body:       `{"jsonrpc":"2.0","id":1,"method":"sampling/createMessage"}`,
			wantStatus: http.StatusOK,
			wantCode:   codeMethodNotFound,
			wantIn:     "tools/call",
		},
		{
			name:       "unknown tool",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"rm_rf"}}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidParams,
			wantIn:     "known_tool",
		},
		{
			name:       "no tool name",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidParams,
			wantIn:     "needs a tool name",
		},
		{
			name:       "arguments are not an object",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"known_tool","arguments":["site/content"]}}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidParams,
			wantIn:     "JSON object",
		},
		{
			name:       "params are not an object",
			body:       `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"known_tool"}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidParams,
			wantIn:     "could not be read",
		},
		{
			name:       "initialize params are not an object",
			body:       `{"jsonrpc":"2.0","id":1,"method":"initialize","params":7}`,
			wantStatus: http.StatusOK,
			wantCode:   codeInvalidParams,
			wantIn:     "could not be read",
		},
	}

	s := testServer(t, testTool("known_tool"))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := post(t, s, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body)
			}
			got := decodeOne(t, rec)
			if got.Error == nil {
				t.Fatalf("result = %s, want an error", got.Result)
			}
			if got.Error.Code != tc.wantCode {
				t.Errorf("code = %d, want %d", got.Error.Code, tc.wantCode)
			}
			if tc.wantIn != "" {
				detail := got.Error.Message + " " + string(got.Error.Data)
				if !strings.Contains(detail, tc.wantIn) {
					t.Errorf("error %q does not carry %q", detail, tc.wantIn)
				}
			}
			// An error that cannot be attributed to a request still needs an
			// id member, spelled null.
			if len(got.ID) == 0 {
				t.Error("the error response has no id member")
			}
		})
	}
}

// A batch answers only the requests in it, in order, and drops the
// notifications.
func TestBatchAnswersRequestsOnly(t *testing.T) {
	s := testServer(t, testTool("a"))
	rec := post(t, s, `[
		{"jsonrpc":"2.0","id":1,"method":"ping"},
		{"jsonrpc":"2.0","method":"notifications/initialized"},
		{"jsonrpc":"2.0","id":2,"method":"tools/list"}
	]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var batch []wire
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("answers = %d, want 2", len(batch))
	}
	if string(batch[0].ID) != "1" || string(batch[1].ID) != "2" {
		t.Fatalf("ids = %s, %s, want 1, 2", batch[0].ID, batch[1].ID)
	}
}

func TestBatchOfNotificationsIsAccepted(t *testing.T) {
	s := testServer(t, testTool("a"))
	rec := post(t, s, `[{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","method":"notifications/cancelled"}]`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want none", rec.Body.String())
	}
}

// One unreadable element must not cost the answers to the readable ones.
func TestBatchWithAnUnreadableElement(t *testing.T) {
	s := testServer(t, testTool("a"))
	rec := post(t, s, `[7,{"jsonrpc":"2.0","id":2,"method":"ping"}]`)

	var batch []wire
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("answers = %d, want 2", len(batch))
	}
	if batch[0].Error == nil || batch[0].Error.Code != codeParseError {
		t.Errorf("first answer = %+v, want a parse error", batch[0])
	}
	if batch[1].Error != nil {
		t.Errorf("second answer = %+v, want the ping result", batch[1])
	}
}

// A body over the limit is refused before anything reads it.
func TestOversizedBodyIsRefused(t *testing.T) {
	s, err := New(Config{
		Name:         "x",
		Tools:        []Tool{testTool("a")},
		Authenticate: StaticBearer("secret", devIdentity),
		MaxBodyBytes: 64,
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a","arguments":{"content":"` +
		strings.Repeat("x", 512) + `"}}}`

	rec := post(t, s, body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	got := decodeOne(t, rec)
	if got.Error == nil || !strings.Contains(got.Error.Message, "too large") {
		t.Fatalf("error = %+v, want it to name the limit", got.Error)
	}
}

// The context a tool gets is the request's, so a client that hangs up stops
// the work it started.
func TestToolSeesTheRequestContext(t *testing.T) {
	tool := testTool("a")
	var cancelled bool
	tool.Call = func(ctx context.Context, id Identity, args json.RawMessage) (Result, error) {
		cancelled = ctx.Err() != nil
		return Text("ok"), nil
	}
	s := testServer(t, tool)

	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"a"}}`))
	req.Header.Set("Authorization", "Bearer secret")
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	s.Handler().ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))

	if !cancelled {
		t.Fatal("the tool was handed a context that is not the request's")
	}
}
