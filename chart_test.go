package siding

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// chartConfigs is where scripts/chart-test.sh leaves the plugin
// configuration of each Middleware the chart rendered from its examples.
const chartConfigs = "dist/chart-configs"

// What the chart renders has to be a configuration the plugin accepts.
// Without the rendered files there is nothing to check: run
// scripts/chart-test.sh, which renders them and then runs this.
func TestChartConfigs(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(chartConfigs, "*.json"))
	if len(files) == 0 {
		t.Skip("no rendered chart configurations; run scripts/chart-test.sh")
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		// Onto the defaults, as Traefik decodes a Middleware's
		// configuration onto what CreateConfig returns.
		cfg := CreateConfig()
		if err := json.Unmarshal(data, cfg); err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		if cfg.Service == "" {
			t.Errorf("%s: the chart set no service", file)
		}
		name := filepath.Base(file)
		if _, err := New(context.Background(), http.NotFoundHandler(), cfg, name); err != nil {
			t.Errorf("%s: the plugin refuses what the chart rendered: %v", file, err)
		}
	}
}
