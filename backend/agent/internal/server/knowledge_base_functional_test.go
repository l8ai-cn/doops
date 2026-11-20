package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The console reaches a knowledge-base document through the public Gateway RPC.
// The Agent is only attached by its outbound reverse tunnel, and the file is
// created on that Agent, not in the Gateway database.
func TestKnowledgeBaseRoundTripThroughReverseGateway(t *testing.T) {
	kb := filepath.Join(t.TempDir(), "kb")
	t.Setenv("DOOPS_KB_ROOT", kb)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	fixture := newExecutionLifecycleFixture(t, GatewayHubOptions{OperationTimeout: 15 * time.Second})
	agent := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	conn := dialFakeGatewayAgent(t, fixture.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.ServeGatewayWebSocketConnWithReady(conn, "kb-functional", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("knowledge base Agent did not stop")
		}
	})
	waitForGatewayAgent(t, fixture.hub, "dev", "local")

	client := fixture.client(t, fixture.admin)
	doc := filepath.Join(kb, "测试.md")
	content := "接口测试 知识库\n"

	lifecycleSend(t, client, 2, "doops_file_write", "kb-interface", map[string]interface{}{
		"path": doc, "content": content,
	})
	writeResult := requireKnowledgeBaseToolResult(t, readKnowledgeBaseMessage(t, client, 2))
	if writeResult["isError"] == true || !strings.Contains(knowledgeBaseResultText(writeResult), "Wrote") {
		t.Fatalf("knowledge base write did not succeed through the gateway: %#v", writeResult)
	}
	onDisk, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("gateway write did not create the agent file: %v", err)
	}
	if string(onDisk) != content {
		t.Fatalf("agent file = %q, want %q", onDisk, content)
	}

	lifecycleSend(t, client, 3, "doops_file_read", "kb-interface", map[string]interface{}{
		"path": doc,
	})
	readResult := requireKnowledgeBaseToolResult(t, readKnowledgeBaseMessage(t, client, 3))
	if readResult["isError"] == true || knowledgeBaseResultText(readResult) != content {
		t.Fatalf("knowledge base read mismatch: %#v", readResult)
	}

	listCommand := fmt.Sprintf("mkdir -p '%s'; ls -1Ap '%s'", kb, kb)
	lifecycleSend(t, client, 4, "doops_shell", "kb-interface", map[string]interface{}{
		"command": listCommand,
	})
	listOutput, listMessage := readKnowledgeBaseExchange(t, client, 4)
	listResult := requireKnowledgeBaseToolResult(t, listMessage)
	if listResult["isError"] == true || !strings.Contains(listOutput, "测试.md") {
		t.Fatalf("knowledge base list did not show the document: output=%q result=%#v", listOutput, listResult)
	}

	lifecycleSend(t, client, 5, "doops_file_write", "kb-interface", map[string]interface{}{
		"path": outside, "content": "pwned",
	})
	rejected := requireKnowledgeBaseToolResult(t, readKnowledgeBaseMessage(t, client, 5))
	if rejected["isError"] != true || !strings.Contains(knowledgeBaseResultText(rejected), "unsafe workspace path") {
		t.Fatalf("absolute path outside the knowledge base was accepted: %#v", rejected)
	}
	untouched, err := os.ReadFile(outside)
	if err != nil || string(untouched) != "secret" {
		t.Fatalf("rejected write changed the outside file: %v %q", err, untouched)
	}

	lifecycleSend(t, client, 6, "doops_shell", "kb-interface", map[string]interface{}{
		"command": fmt.Sprintf("rm -f '%s'", doc),
	})
	_, deleteMessage := readKnowledgeBaseExchange(t, client, 6)
	if deleteResult := requireKnowledgeBaseToolResult(t, deleteMessage); deleteResult["isError"] == true {
		t.Fatalf("knowledge base delete failed: %#v", deleteResult)
	}
	if _, err := os.Stat(doc); !os.IsNotExist(err) {
		t.Fatalf("delete did not remove the knowledge base document: %v", err)
	}
}

func requireKnowledgeBaseToolResult(t *testing.T, response map[string]interface{}) map[string]interface{} {
	t.Helper()
	if response["error"] != nil {
		t.Fatalf("expected a terminal tool result, got RPC error: %#v", response)
	}
	result, ok := response["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing terminal tool result: %#v", response)
	}
	return result
}

func readKnowledgeBaseMessage(t *testing.T, client *websocket.Conn, id int) map[string]interface{} {
	t.Helper()
	_, response := readKnowledgeBaseExchange(t, client, id)
	return response
}

func readKnowledgeBaseExchange(t *testing.T, client *websocket.Conn, id int) (string, map[string]interface{}) {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer client.SetReadDeadline(time.Time{})
	var output strings.Builder
	for {
		var msg map[string]interface{}
		if err := client.ReadJSON(&msg); err != nil {
			t.Fatalf("read gateway response: %v", err)
		}
		if method, _ := msg["method"].(string); method == "notifications/message" {
			output.WriteString(notificationData(msg))
			continue
		}
		if msgID, _ := msg["id"].(float64); msgID == float64(id) {
			return output.String(), msg
		}
	}
}

func knowledgeBaseResultText(result map[string]interface{}) string {
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		return ""
	}
	item, _ := content[0].(map[string]interface{})
	text, _ := item["text"].(string)
	return text
}
