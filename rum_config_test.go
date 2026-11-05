package rumcollector

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/highemerly/rum-collector/internal/config"
)

// assetPath is the copy embedded in the binary and used by local dev and the
// E2E smoke test. Production mounts the same JSON from a ConfigMap kept in the
// (private) manifest repository; this test cannot see that copy, so mirror any
// change there by hand.
const assetPath = "assets/rum-config.json"

// TestEveryServiceHasPathGroups catches the specific mistake that made the
// deployed config useless: path_groups is keyed by service *id*, not by host, so
// a key that no service maps to is silently dead and its service silently falls
// back to the "*" rules. Neither is an error the collector can report.
func TestEveryServiceHasPathGroups(t *testing.T) {
	raw, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Compile(raw); err != nil {
		t.Fatalf("%s does not compile: %v", assetPath, err)
	}

	var doc struct {
		Services   map[string]string `json:"services"`
		PathGroups map[string]any    `json:"path_groups"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	ids := make(map[string]struct{}, len(doc.Services))
	for host, id := range doc.Services {
		ids[id] = struct{}{}
		if _, ok := doc.PathGroups[id]; !ok {
			t.Errorf("service %q (%s) has no path_groups entry: every path would fall back to the \"*\" rules", id, host)
		}
	}
	for key := range doc.PathGroups {
		if key == "*" {
			continue
		}
		if _, ok := ids[key]; !ok {
			t.Errorf("path_groups key %q matches no service id: the rules are dead", key)
		}
	}
}
