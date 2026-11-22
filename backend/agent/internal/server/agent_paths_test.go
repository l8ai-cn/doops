package server

import (
	"encoding/json"
	"testing"
)

func TestAgentNodeInfoPaths(t *testing.T) {
	for _, tc := range []struct{ name, workspace, kb, wantWorkspace, wantKB string }{
		{"defaults", "", "", "/root/ws", "/root/kb"},
		{"custom", "/home/ops/ws/", "/home/ops/kb/", "/home/ops/ws", "/home/ops/kb"},
		{"whitespace", "  /data/ws  ", "  /data/kb  ", "/data/ws", "/data/kb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOOPS_WORKSPACE_ROOT", tc.workspace)
			t.Setenv("DOOPS_KB_ROOT", tc.kb)
			gw := NewGateway()
			ts := newGatewayAuthorizedAgentServer(t, gw)
			defer ts.Close()
			conn := dialAgentTestWS(t, ts.URL)
			defer conn.Close()
			reply := callToolResult(t, conn, "doops_node_info", map[string]interface{}{"session_id": "paths-test", "section": "paths"})
			result := reply["result"].(map[string]interface{})
			content := result["content"].([]interface{})[0].(map[string]interface{})["text"].(string)
			var paths map[string]string
			if err := json.Unmarshal([]byte(content), &paths); err != nil {
				t.Fatal(err)
			}
			if paths["workspace_root"] != tc.wantWorkspace || paths["kb_root"] != tc.wantKB {
				t.Fatalf("paths = %#v", paths)
			}
		})
	}
}

func TestAgentNodeInfoPathsRejectsInvalidKBRoot(t *testing.T) {
	t.Setenv("DOOPS_KB_ROOT", "relative")
	ts := newGatewayAuthorizedAgentServer(t, NewGateway())
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	reply := callToolResult(t, conn, "doops_node_info", map[string]interface{}{"session_id": "paths-test", "section": "paths"})
	result := reply["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("expected error, got %#v", reply)
	}
}
