package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func extServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{
		Name:    "prodeko-content-editor",
		Version: "test",
		Tools: func() []Tool {
			tool := testTool("noop")
			tool.Meta = json.RawMessage(`{"ui":{"resourceUri":"ui://prodeko-editor/upload"}}`)
			return []Tool{tool}
		}(),
		Authenticate: StaticBearer("secret", devIdentity),
		Prompts: []Prompt{{
			Name:        "post-announcement",
			Description: "Post a news item",
			Text:        "Open the announcements file and follow the conventions.",
		}},
		Resources: []Resource{{
			URI:      "ui://prodeko-editor/upload",
			Name:     "Image upload",
			MimeType: "text/html",
			Text:     "<!doctype html><title>upload</title>",
			Meta:     json.RawMessage(`{"ui":{"csp":{"connectDomains":["https://mcp.prodeko.org"]}}}`),
		}},
		Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestInitializeAdvertisesPromptsAndResourcesOnlyWhenPresent(t *testing.T) {
	bare := post(t, testServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if body := bare.Body.String(); strings.Contains(body, `"prompts"`) || strings.Contains(body, `"resources"`) {
		t.Errorf("a server with none advertises them: %s", body)
	}
	full := post(t, extServer(t), `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)
	if body := full.Body.String(); !strings.Contains(body, `"prompts"`) || !strings.Contains(body, `"resources"`) {
		t.Errorf("a server with both advertises neither: %s", body)
	}
}

func TestPromptsListAndGet(t *testing.T) {
	s := extServer(t)
	list := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"prompts/list"}`)
	if body := list.Body.String(); !strings.Contains(body, "post-announcement") {
		t.Errorf("prompts/list = %s", body)
	}
	got := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"post-announcement"}}`)
	if body := got.Body.String(); !strings.Contains(body, "announcements file") || !strings.Contains(body, `"role":"user"`) {
		t.Errorf("prompts/get = %s", body)
	}
	missing := post(t, s, `{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"nope"}}`)
	if body := missing.Body.String(); !strings.Contains(body, "unknown prompt") {
		t.Errorf("prompts/get of an unknown prompt = %s", body)
	}
}

func TestResourcesListAndReadCarryTheUIMeta(t *testing.T) {
	s := extServer(t)
	list := post(t, s, `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`)
	if body := list.Body.String(); !strings.Contains(body, "ui://prodeko-editor/upload") {
		t.Errorf("resources/list = %s", body)
	}
	read := post(t, s, `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"ui://prodeko-editor/upload"}}`)
	body := read.Body.String()
	// encoding/json writes < as <, so the marker avoids angle brackets.
	for _, want := range []string{"doctype html", "connectDomains", `"_meta"`} {
		if !strings.Contains(body, want) {
			t.Errorf("resources/read omits %q: %s", want, body)
		}
	}
	missing := post(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/read","params":{"uri":"ui://nope"}}`)
	if !strings.Contains(missing.Body.String(), "unknown resource") {
		t.Errorf("resources/read of an unknown uri = %s", missing.Body.String())
	}
}

func TestToolsListCarriesTheToolMeta(t *testing.T) {
	got := post(t, extServer(t), `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if body := got.Body.String(); !strings.Contains(body, `"_meta"`) || !strings.Contains(body, "resourceUri") {
		t.Errorf("tools/list = %s", body)
	}
}
