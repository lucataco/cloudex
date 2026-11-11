package deploy

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
)

// What cloudex deploy uploads must declare the same Durable Objects and
// variables as apps/api/wrangler.jsonc, which wrangler dev and the bundle use:
// a class in one and not the other works in development and breaks deploys,
// or the other way round.
func TestWorkerMetadataMatchesWrangler(t *testing.T) {
	raw, err := os.ReadFile("../../../apps/api/wrangler.jsonc")
	if err != nil {
		t.Fatal(err)
	}
	// Whole-line comments only, as scripts/bundle-api.sh reads it.
	src := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAll(raw, nil)
	var w struct {
		DurableObjects struct {
			Bindings []struct {
				Name      string `json:"name"`
				ClassName string `json:"class_name"`
			} `json:"bindings"`
		} `json:"durable_objects"`
		Exports    map[string]map[string]string `json:"exports"`
		Vars       map[string]string            `json:"vars"`
		Containers []struct {
			ClassName string `json:"class_name"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(src, &w); err != nil {
		t.Fatalf("wrangler.jsonc: %v", err)
	}

	md := workerMetadata("2026-10-01", nil, "img", "node-img", "db", "", "k", true)
	var mdDOs, wDOs, mdVars, wVars []string
	for _, b := range md["bindings"].([]map[string]any) {
		switch b["type"] {
		case "durable_object_namespace":
			mdDOs = append(mdDOs, b["name"].(string)+"="+b["class_name"].(string))
		case "plain_text":
			mdVars = append(mdVars, b["name"].(string))
		}
	}
	for _, b := range w.DurableObjects.Bindings {
		wDOs = append(wDOs, b.Name+"="+b.ClassName)
	}
	for k := range w.Vars {
		wVars = append(wVars, k)
	}
	sort.Strings(mdDOs)
	sort.Strings(wDOs)
	sort.Strings(mdVars)
	sort.Strings(wVars)
	if !reflect.DeepEqual(mdDOs, wDOs) {
		t.Errorf("Durable Object bindings: deploy %v, wrangler.jsonc %v", mdDOs, wDOs)
	}
	if !reflect.DeepEqual(mdVars, wVars) {
		t.Errorf("variables: deploy %v, wrangler.jsonc %v", mdVars, wVars)
	}
	exports, _ := json.Marshal(md["exports"])
	var mdExports map[string]map[string]string
	json.Unmarshal(exports, &mdExports)
	if !reflect.DeepEqual(mdExports, w.Exports) {
		t.Errorf("exports: deploy %v, wrangler.jsonc %v", mdExports, w.Exports)
	}
	if len(w.Containers) != 1 || w.Containers[0].ClassName != md["containers"].([]any)[0].(map[string]any)["class_name"] {
		t.Errorf("containers: wrangler.jsonc %v", w.Containers)
	}
	for _, b := range md["bindings"].([]map[string]any) {
		if b["name"] == "NODE_IMAGE" && b["text"] != "node-img" {
			t.Errorf("NODE_IMAGE %v", b["text"])
		}
	}
}

func TestNodeImageIsThePublishedReferenceAtTheCopiedDigest(t *testing.T) {
	for src, want := range map[string]string{
		"ghcr.io/lucataco/cloudex-base:v0.2.2":                            "ghcr.io/lucataco/cloudex-base:v0.2.2@sha256:abc",
		"ghcr.io/lucataco/cloudex-base@sha256:" + strings.Repeat("1", 64): "ghcr.io/lucataco/cloudex-base@sha256:abc",
		"ghcr.io/lucataco/cloudex-base":                                   "ghcr.io/lucataco/cloudex-base:latest@sha256:abc",
	} {
		ref, err := name.ParseReference(src)
		if err != nil {
			t.Fatal(err)
		}
		if got := nodeImageRef(ref, "sha256:abc"); got != want {
			t.Errorf("%s: %s, want %s", src, got, want)
		}
	}
}
