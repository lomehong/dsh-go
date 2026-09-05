package preset

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestProbeStandardRows(t *testing.T) {
	base := `E:\Development\Code\go\dsh-go\webassets\node_modules\@deepseek-ai\dsh-agent-presets\presets`
	agentPreset := AgentPreset{ID: "standard", Path: filepath.Join(base, "standard", "agent.cordis.yml")}
	rows, err := CompositionRows(agentPreset)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	kinds := map[string]int{}
	for _, row := range rows {
		switch {
		case row.Enabled == nil:
			kinds["conditional"]++
		case *row.Enabled:
			kinds["enabled"]++
		default:
			kinds["disabled"]++
		}
	}
	fmt.Println("distribution:", kinds, "total:", len(rows))
}
