package driver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	ap "github.com/inference-sh/agentprotocol"
	"github.com/inference-sh/agentprotocol/codexapp"
)

func TestCallerToolImagesReachMCPContent(t *testing.T) {
	server, err := callerToolServer([]ap.Tool{{Type: "function", Function: ap.ToolFunction{Name: "shot"}}},
		func(context.Context, ToolCall) (ToolOutput, error) {
			return ToolOutput{Text: "a screenshot", Images: []ToolImage{{MimeType: "image/jpeg", Data: []byte("jpg")}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := server.Handle(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shot"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(raw); !strings.Contains(s, `"text":"a screenshot"`) || !strings.Contains(s, `"type":"image"`) || !strings.Contains(s, `"mimeType":"image/jpeg"`) || !strings.Contains(s, `"data":"anBn"`) {
		t.Errorf("tools/call = %s", raw)
	}
}

func TestCodexToolResultSendsImagesAsDataURLs(t *testing.T) {
	res := codexToolResult(ToolOutput{Text: "look", Images: []ToolImage{{MimeType: "image/png", Data: []byte{1, 2, 3}}}, IsError: true})
	if res.Success || len(res.ContentItems) != 2 {
		t.Fatalf("response = %+v", res)
	}
	if txt, ok := res.ContentItems[0].AsInputTextDynamicToolCallOutputContentItem(); !ok || txt.Text != "look" {
		t.Errorf("first item = %+v", res.ContentItems[0])
	}
	img, ok := res.ContentItems[1].AsInputImageDynamicToolCallOutputContentItem()
	if !ok || img.ImageURL != "data:image/png;base64,AQID" || img.Type != codexapp.DynamicToolCallOutputContentItemTypeInputImage {
		t.Errorf("image item = %+v", res.ContentItems[1])
	}
	if only := codexToolResult(ToolOutput{Images: []ToolImage{{MimeType: "image/png", Data: []byte{1}}}}); len(only.ContentItems) != 1 || !only.Success {
		t.Errorf("image-only response = %+v", only)
	}
	if empty := codexToolResult(ToolOutput{}); len(empty.ContentItems) != 1 {
		t.Errorf("empty response = %+v", empty)
	}
}
