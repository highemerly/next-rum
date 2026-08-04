package rumcollector

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/highemerly/rum-collector/internal/config"
)

const (
	assetPath     = "assets/rum-config.json"
	configMapPath = "deploy/k8s/configmap.yaml"
	blockMarker   = "rum-config.json: |\n"
	blockIndent   = "    "
)

// extractConfigMapJSON pulls the literal block that follows `rum-config.json: |`
// out of the ConfigMap. A YAML parser would be a heavier dependency than this
// file's fixed shape warrants: the block is every following line that is blank
// or indented by blockIndent.
func extractConfigMapJSON(t *testing.T, yaml string) string {
	t.Helper()
	_, rest, found := strings.Cut(yaml, blockMarker)
	if !found {
		t.Fatalf("%s: no %q block; did the ConfigMap key change?", configMapPath, blockMarker)
	}
	var out []string
	for _, line := range strings.Split(rest, "\n") {
		if strings.TrimSpace(line) == "" {
			out = append(out, "")
			continue
		}
		if !strings.HasPrefix(line, blockIndent) {
			break
		}
		out = append(out, strings.TrimPrefix(line, blockIndent))
	}
	return strings.Join(out, "\n")
}

// TestConfigMapMatchesAsset keeps the two in-repo copies of rum-config.json in
// lockstep. The file is duplicated because assets/ is embedded in the binary
// (and used for local dev) while the ConfigMap is what production mounts. A
// silent divergence is expensive to spot: it surfaces only as an empty or
// wrongly-grouped dashboard, never as an error. This has already bitten once —
// the deployed copy carried a path_groups key that no entry in services mapped
// to, so every page fell back to the "*" rules.
//
// Note there is a third copy in k8sg1-repo (manifests/rum/prd/configmap.yaml)
// that this test cannot see. Mirror any change there too.
func TestConfigMapMatchesAsset(t *testing.T) {
	assetRaw, err := os.ReadFile(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	cmRaw, err := os.ReadFile(configMapPath)
	if err != nil {
		t.Fatal(err)
	}
	embedded := extractConfigMapJSON(t, string(cmRaw))

	var fromAsset, fromConfigMap any
	if err := json.Unmarshal(assetRaw, &fromAsset); err != nil {
		t.Fatalf("%s is not valid JSON: %v", assetPath, err)
	}
	if err := json.Unmarshal([]byte(embedded), &fromConfigMap); err != nil {
		t.Fatalf("%s embedded JSON is invalid: %v", configMapPath, err)
	}

	if !reflect.DeepEqual(fromAsset, fromConfigMap) {
		t.Errorf("%s and %s have diverged; copy one over the other (and mirror it to k8sg1-repo)", assetPath, configMapPath)
	}
}

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
