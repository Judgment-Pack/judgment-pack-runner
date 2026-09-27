// Test-only MCP source. The barrier lets integration tests stop Runner while a
// real Gateway/adapter/source process chain continues working.
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

func main() {
	dir := os.Args[1]
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var m struct {
			ID     json.RawMessage
			Method string
		}
		if json.Unmarshal(in.Bytes(), &m) != nil || len(m.ID) == 0 {
			continue
		}
		var result any
		switch m.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "durable-fixture", "version": "0.1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			f, e := os.OpenFile(filepath.Join(dir, "calls"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				panic(e)
			}
			f.WriteString("call\n")
			f.Close()
			if _, e = os.Stat(filepath.Join(dir, "hold")); e == nil {
				os.WriteFile(filepath.Join(dir, "started"), []byte("waiting"), 0600)
				for {
					if _, e = os.Stat(filepath.Join(dir, "release")); e == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			var input any
			json.Unmarshal([]byte(`{"facts":{"request":{"type":"data-access","completeness":"complete","appropriateness":"pass","embargoedInformationToUnauthorizedRecipients":false}},"evidence":{"intake-form":"present","sponsor-endorsement":"present"}}`), &input)
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "verified input"}}, "structuredContent": input}
		default:
			continue
		}
		b, e := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
		if e != nil {
			panic(e)
		}
		os.Stdout.Write(append(b, '\n'))
	}
}
